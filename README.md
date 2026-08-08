# gomongoapi

[![Go Reference](https://pkg.go.dev/badge/github.com/alexland23/gomongoapi.svg)](https://pkg.go.dev/github.com/alexland23/gomongoapi)
[![CI](https://github.com/alexland23/gomongoapi/actions/workflows/ci.yml/badge.svg)](https://github.com/alexland23/gomongoapi/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/alexland23/gomongoapi/branch/main/graph/badge.svg)](https://codecov.io/gh/alexland23/gomongoapi)

`gomongoapi` is a pure Go package for standing up an HTTP server that exposes a MongoDB
instance over a small set of REST routes. It was built to make it easy to point
Grafana at MongoDB — spin up the server, point a Grafana datasource at it, and start
building dashboards without writing any query/API glue yourself.

The server is built on [gin](https://github.com/gin-gonic/gin) and the official
[MongoDB Go driver](https://go.mongodb.org/mongo-driver), and the underlying `gin.Engine`
can be fully customized or replaced.

Recommended Grafana plugins to query these routes:
[JSON API](https://grafana.com/grafana/plugins/marcusolsson-json-datasource/) and
[Infinity](https://grafana.com/grafana/plugins/yesoreyeram-infinity-datasource/).

## Install

```sh
go get github.com/alexland23/gomongoapi
```

## Quick start

```go
package main

import (
	"log"

	"github.com/alexland23/gomongoapi"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	// Set server options
	serverOpts := gomongoapi.ServerOptions()
	serverOpts.SetMongoClientOpts(options.Client().ApplyURI("mongodb://localhost:27017"))
	serverOpts.SetDefaultDB("app")
	serverOpts.SetAddress(":8080") // listen on all interfaces; default is localhost-only, see Security

	// Create server
	server := gomongoapi.NewServer(serverOpts)

	// Start server, blocks until SIGINT/SIGTERM triggers a graceful shutdown
	// or an error occurs
	log.Fatal(server.Start())
}
```

## Examples

[`examples/simpleDemo`](examples/simpleDemo) is a runnable, `docker-compose`-based demo:
it starts MongoDB, seeds it with sample data, runs a `gomongoapi` server (including a
custom route), and provisions a Grafana instance with the JSON API and Infinity
datasources already pointed at it — so you can go straight to building dashboards
against Mongo data.

```sh
cd examples/simpleDemo
docker-compose up
```

Then open Grafana at http://localhost:3000 (default login `admin` / `admin`).

> **Not for production.** This demo runs `/api` with no auth middleware and Grafana on
> the default `admin` / `admin` login, all published on the host network. It's shaped for
> a quick local look, not as a template for a real deployment — see [Security](#security).

## Security

`gomongoapi` gives a caller query — and, if misconfigured, write — access to whatever
Mongo credentials you configure it with. Several of the sharpest edges are restricted by
default (below), but none of that is a substitute for scoping the credentials themselves.
Treat this as a query surface with write potential, not an inherently read-only reporting
endpoint.

### Enforced by the library

- **`$where`/`$function`/`$out`/`$merge` are rejected by default.** Every `find`/`count`
  filter and `aggregate` pipeline is scanned recursively — including inside `$and`/`$or`/
  `$expr` and nested pipeline stages — for these four operators, and rejected with `400` if
  found. `$where`/`$function` can execute server-side JavaScript depending on your MongoDB
  version and configuration; `$out`/`$merge` can write to or overwrite a collection through
  what looks like a query API. Set `Options.AllowUnsafeOperators` (`SetAllowUnsafeOperators(true)`)
  to disable this if you trust your callers and have a legitimate need for one of these.
- **Not reachable off the host by default.** `Address` defaults to `localhost:8080`.
  Binding to a non-loopback address (`:8080`, `0.0.0.0:8080`, etc. — needed for Docker port
  publishing, see [`examples/simpleDemo`](examples/simpleDemo)) is an explicit opt-in via
  `SetAddress`.
- **Best-effort read-only credential warning.** On `Start()`, `gomongoapi` runs Mongo's
  `connectionStatus` command and logs a warning if the connected user holds any write
  privilege. This is advisory only: it never blocks startup, and a failure to run the check
  (no auth configured, a custom role that disallows `connectionStatus`, etc.) is silently
  ignored. **It is not a substitute for actually using a read-only user** — see below —
  only a nudge in case you forgot.
- **Request body size limit.** `/api` request bodies are capped at `MaxBodyBytes` (default
  1 MiB) via `http.MaxBytesReader`, so a large `find`/`count`/`aggregate` filter or
  pipeline can't be used to pressure server memory. A body over the limit gets a `413`
  response. Adjust it with `SetMaxBodyBytes`, or pass `0` to disable the limit.

### Still the consumer's responsibility

#### Use a read-only Mongo user

Point `MongoClientOpts` at a connection string scoped to a Mongo user with `read` (not
`readWrite`) on only the databases you intend to expose. This is the control that actually
stops write access from doing damage — the operator restrictions and the startup warning
above are defense in depth on top of it, not a replacement for it: they only catch known
dangerous operators and known write actions, not every way write-capable credentials could
be misused.

#### No built-in auth beyond an optional shared API key

`gomongoapi` does not authenticate requests on its own beyond one convenience helper.
`SetAPIKey` installs a check requiring a matching `X-API-Key` header on every `/api`
request:

```go
server.SetAPIKey("replace-with-a-securely-generated-key") // load from env/secrets, don't hardcode
```

For anything beyond a single shared secret (per-caller keys, JWT, OAuth), use
`SetAPIMiddleware` to add your own check in front of every `/api` route instead:

```go
const apiKey = "replace-with-a-securely-generated-key" // load from env/secrets, don't hardcode

server.SetAPIMiddleware(func(ctx *gin.Context) {
	if ctx.GetHeader("X-API-Key") != apiKey {
		ctx.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	ctx.Next()
})
```

`SetCustomMiddleware` is the equivalent hook for the `/custom` route group; apply the
same kind of check there if you add custom routes.

**Call these before `Start()`.** gin composes a route's handler chain when the route is
registered, and `/api`/`/custom` routes are registered inside `Start()`. Calling
`SetAPIKey`/`SetAPIMiddleware`/`SetCustomMiddleware` after `Start()` has begun (e.g. from
another goroutine) won't protect the routes already registered — since this is typically
an auth hook, a silent no-op there would fail open, so `gomongoapi` logs a warning
instead. Set all middleware between `NewServer()` and `Start()`.

#### Network boundary beyond localhost

The `localhost`-only default keeps the server off the network entirely, but once you opt
into a non-loopback `Address` (e.g. for Docker), don't publish the port directly to the
internet. Put it behind a reverse proxy (for TLS termination and a single controlled entry
point) or keep it on a private network reachable only by the Grafana instance querying it
— ideally both.

#### Rate limiting

`gomongoapi` doesn't ship any rate limiting. If the server is reachable by more than a
trusted internal caller, add a rate-limiting gin middleware via `SetAPIMiddleware`, for
example using [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate)
directly (no extra dependency beyond the extended standard library) or a package like
[`ulule/limiter`](https://github.com/ulule/limiter):

```go
limiter := rate.NewLimiter(rate.Limit(10), 20) // 10 requests/sec, burst of 20

server.SetAPIMiddleware(func(ctx *gin.Context) {
	if !limiter.Allow() {
		ctx.AbortWithStatus(http.StatusTooManyRequests)
		return
	}
	ctx.Next()
})
```

## Default routes

| Path                                | HTTP Verb | Body  | Result                                                                                                |
| ------------------------------------ | --------- | ----- | ------------------------------------------------------------------------------------------------------ |
| `/`                                   | GET       | Empty | Always 200, used to test the connection (liveness).                                                    |
| `/api/health`                         | GET       | Empty | Pings MongoDB; 200 if reachable, 503 otherwise (readiness).                                             |
| `/api/databases`                      | GET       | Empty | Returns list of available databases, unless a default database is set.                                 |
| `/api/collections`                    | GET       | Empty | Returns a list of collections for the default db or the one passed in the `database` url param.       |
| `/api/collections/:name/find`         | POST      | JSON  | Returns the result of a find on collection `:name`. DB is either the default or `database` url param.  |
| `/api/collections/:name/count`        | POST      | JSON  | Returns the count of matching documents in collection `:name`. DB is either the default or `database` url param. |
| `/api/collections/:name/aggregate`    | POST      | JSON  | Returns the result of an aggregate on collection `:name`. DB is either the default or `database` url param. |
| `/custom/<custom route>`              | GET       | N/A   | User-defined GET route — user controls everything.                                                     |
| `/custom/<custom route>`              | POST      | N/A   | User-defined POST route — user controls everything.                                                    |

`find` and `aggregate` both accept an optional `limit` url param, and both respect a
server-wide default (`FindLimit`) and max (`FindMaxLimit`) if configured (see
[`Options`](#options) below). For `aggregate`, the limit is applied as a `$limit` stage
appended to the end of the caller's pipeline.

`find` additionally accepts:

- `skip` url param — a non-negative int, mapped to `options.Find().SetSkip`.
- `sort` url param — a JSON object url-encoded into the query string, e.g.
  `?sort={"createdAt":-1}`, mapped to `options.Find().SetSort`. A JSON sort spec doesn't fit
  a plain scalar query param, so it travels as an encoded JSON string like this rather than
  as nested query keys.
- `projection` — not a url param, but a top-level key in the request body (matched
  case-insensitively), mapped to `options.Find().SetProjection`. Everything else in the body
  is the filter, e.g. `{"UserName": "Jon", "projection": {"UserName": 1}}` filters on
  `UserName` and returns only that field. This makes `projection` a reserved top-level
  filter key — a genuine field named `projection` can't be filtered on directly through this
  route.

### Error responses

Every `/api/...` route returns a JSON envelope on failure, `{"error": "<message>"}`,
with a non-2xx status code — for example a missing `database` param on
`/api/collections` returns `400` with body `{"error": "Database name was not passed, one is needed"}`.
This applies to validation errors (bad input) as well as errors surfaced from MongoDB
itself (e.g. connection failures). See [CHANGELOG.md](CHANGELOG.md) for the release this
landed in — earlier versions returned a plain-text body instead.

## Options

`gomongoapi.ServerOptions()` returns an `*Options` populated with defaults, which can then
be customized with the setter methods below before being passed to `gomongoapi.NewServer()`.

| Field / Setter                                 | Default            | Description                                                                                   |
| ----------------------------------------------- | ------------------- | ----------------------------------------------------------------------------------------------- |
| `Router` / `SetRouter(*gin.Engine)`             | `gin.Default()`     | The gin engine the server will use. Pass your own to fully customize the server.               |
| `Address` / `SetAddress(string)`                | `localhost:8080`     | Address the server listens on. See [Security](#security) for why this defaults to loopback-only. |
| `CustomRouteName` / `SetCustomRouteName(string)`| `custom`             | Route group name used for custom routes. Cannot be `/` or `/api`.                               |
| `MongoClientOpts` / `SetMongoClientOpts(*options.ClientOptions)` | empty options | MongoDB client options, e.g. connection URI. Required for a real connection.        |
| `DefaultDB` / `SetDefaultDB(string)`            | none (unset)         | If set, all routes operate against this database and the `database` url param is not needed.    |
| `FindLimit` / `SetFindLimit(int)`               | `1000`               | Default number of records returned by `find` and `aggregate` when no `limit` url param is passed. |
| `FindMaxLimit` / `SetFindMaxLimit(int)`         | `0` (no limit)       | Upper bound on the `limit` url param for `find` and `aggregate`. Requests above this are rejected. |
| `HealthCheckTimeout` / `SetHealthCheckTimeout(time.Duration)` | `5s`  | Upper bound on the Mongo ping issued by `/api/health`.                                          |
| `MaxBodyBytes` / `SetMaxBodyBytes(int64)`       | `1 MiB` (`1048576`) | Upper bound on `/api` request bodies (find/count/aggregate). Requests over this return `413`. `0` disables the limit. |
| `AllowUnsafeOperators` / `SetAllowUnsafeOperators(bool)` | `false` | If `false`, `find`/`count` filters and `aggregate` pipelines containing `$where`, `$function`, `$out`, or `$merge` are rejected with `400`. See [Security](#security). |

## Custom routes and middleware

The `Server` interface exposes hooks for adding your own routes and middleware, and for
getting at the underlying Mongo client. `SetAPIMiddleware`/`SetCustomMiddleware` must be
called before `Start()` — see [No built-in auth](#no-built-in-auth) for why:

```go
// Add middleware to the /api route group (logging, auth, etc.)
server.SetAPIMiddleware(myMiddleware)

// Add middleware to the /custom route group
server.SetCustomMiddleware(myMiddleware)

// Add a custom GET route under /custom
server.AddCustomGET("/appUsersCount", func(ctx *gin.Context) {
	client := server.GetMongoClient()

	count, err := client.Database("app").Collection("users").CountDocuments(ctx.Request.Context(), bson.M{})
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, bson.M{"error": "Error running count: " + err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, bson.M{"Count": count})
})

// Add a custom POST route under /custom
server.AddCustomPOST("/myRoute", myHandler)
```

## License

[MIT](LICENSE)
