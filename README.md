# gomongoapi

[![Go Reference](https://pkg.go.dev/badge/github.com/alexland23/gomongoapi.svg)](https://pkg.go.dev/github.com/alexland23/gomongoapi)
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
	serverOpts.SetAddress(":8080")

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

`gomongoapi` gives a caller direct, unauthenticated access to whatever Mongo credentials
you configure it with. It has no built-in auth, and the API is **not read-only**:
`aggregate` passes the caller-supplied pipeline straight to the driver with no stage
filtering, so a pipeline containing `$out` or `$merge` can write to or overwrite
collections, and `find`/`count` filters are passed through as-is, so operators like
`$where` or `$function` can execute server-side JavaScript depending on your MongoDB
version and configuration. Treat this as a query surface with write potential, not a
read-only reporting endpoint, and take the following seriously before exposing it
anywhere besides `localhost`.

### Use a read-only Mongo user

Point `MongoClientOpts` at a connection string scoped to a Mongo user with `read` (not
`readWrite`) on only the databases you intend to expose. This is the control that
actually stops `$out`/`$merge`/`$where` from doing damage — everything else here is
defense in depth on top of it.

### No built-in auth

`gomongoapi` does not authenticate requests itself. Use `SetAPIMiddleware` to add your
own check in front of every `/api` route, for example a shared API key:

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

### Network boundary

Don't publish the server's port directly to the internet. Put it behind a reverse proxy
(for TLS termination and a single controlled entry point) or keep it on a private
network reachable only by the Grafana instance querying it — ideally both.

### Rate limiting

`gomongoapi` doesn't ship any rate limiting. If the server is reachable by more than a
trusted internal caller, add a rate-limiting gin middleware (e.g.
[`ulule/limiter`](https://github.com/ulule/limiter) or
[`gin-contrib/timeout`](https://github.com/gin-contrib/timeout)-style approaches) via
`SetAPIMiddleware`.

## Default routes

| Path                                | HTTP Verb | Body  | Result                                                                                                |
| ------------------------------------ | --------- | ----- | ------------------------------------------------------------------------------------------------------ |
| `/`                                   | GET       | Empty | Always 200, used to test the connection.                                                               |
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

## Options

`gomongoapi.ServerOptions()` returns an `*Options` populated with defaults, which can then
be customized with the setter methods below before being passed to `gomongoapi.NewServer()`.

| Field / Setter                                 | Default            | Description                                                                                   |
| ----------------------------------------------- | ------------------- | ----------------------------------------------------------------------------------------------- |
| `Router` / `SetRouter(*gin.Engine)`             | `gin.Default()`     | The gin engine the server will use. Pass your own to fully customize the server.               |
| `Address` / `SetAddress(string)`                | `:8080`              | Address the server listens on.                                                                  |
| `CustomRouteName` / `SetCustomRouteName(string)`| `custom`             | Route group name used for custom routes. Cannot be `/` or `/api`.                               |
| `MongoClientOpts` / `SetMongoClientOpts(*options.ClientOptions)` | empty options | MongoDB client options, e.g. connection URI. Required for a real connection.        |
| `DefaultDB` / `SetDefaultDB(string)`            | none (unset)         | If set, all routes operate against this database and the `database` url param is not needed.    |
| `FindLimit` / `SetFindLimit(int)`               | `1000`               | Default number of records returned by `find` and `aggregate` when no `limit` url param is passed. |
| `FindMaxLimit` / `SetFindMaxLimit(int)`         | `0` (no limit)       | Upper bound on the `limit` url param for `find` and `aggregate`. Requests above this are rejected. |

## Custom routes and middleware

The `Server` interface exposes hooks for adding your own routes and middleware, and for
getting at the underlying Mongo client:

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
		ctx.String(http.StatusInternalServerError, "Error running count: "+err.Error())
		return
	}

	ctx.JSON(http.StatusOK, bson.M{"Count": count})
})

// Add a custom POST route under /custom
server.AddCustomPOST("/myRoute", myHandler)
```

## License

[MIT](LICENSE)
