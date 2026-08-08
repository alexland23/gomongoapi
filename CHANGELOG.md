# Changelog

All notable changes to this project are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [1.2.0] - 2026-08-08

### Added

- `Server.AddCustomRoute(method, relativePath string, handlers ...gin.HandlerFunc)` registers
  a route under the `/custom` group for any HTTP method (`PUT`, `DELETE`, `PATCH`, etc.), not
  just `GET`/`POST`. `AddCustomGET`/`AddCustomPOST` now delegate to it instead of duplicating
  the registration call. (#36)
- `example_test.go` with `ExampleNewServer` and `ExampleServer_AddCustomGET`, so pkg.go.dev
  renders the README quick-start and custom-route snippets as runnable documentation. (#37)
- `CONTRIBUTING.md`, a CI badge, and `scripts/release.sh` (tags a release, pushes it, creates
  the GitHub release from the matching `CHANGELOG.md` section, and closes the milestone). (#38)

### Changed

- **Breaking:** migrated from `go.mongodb.org/mongo-driver` (v1, now in maintenance mode) to
  `go.mongodb.org/mongo-driver/v2` throughout the library, test suite, `example_test.go`, and
  `examples/simpleDemo`. The only source-level change required downstream is `mongo.Connect`,
  which drops its `context.Context` parameter in v2 (`mongo.Connect(opts...)` instead of
  `mongo.Connect(ctx, opts...)`); connection establishment is still bounded by
  `Options.ConnectTimeout` via the subsequent `Ping`. `bson.M`/`bson.D`/`bson.E`, the
  `options.*` builders, and all other call sites are source-compatible between v1 and v2. (#58)

## [1.1.0] - 2026-08-08

### Added

- `/api/health` readiness probe: `GET` pings MongoDB with a bounded timeout and
  returns `200 {"status":"ok"}` when reachable, `503 {"status":"error","error":"..."}`
  otherwise. Bounded by the new `Options.HealthCheckTimeout` /
  `SetHealthCheckTimeout(time.Duration)` (default `5s`). `/` is unchanged and remains
  an unconditional liveness check. (#34)
- `find` now supports `sort` and `skip` url params, and a `projection` field in the
  request body, mapping to `options.Find().SetSort`/`SetSkip`/`SetProjection`
  respectively. `sort` is a JSON object url-encoded into the query string (e.g.
  `?sort={"createdAt":-1}`); `projection` is a reserved top-level key in the request
  body, matched case-insensitively, with the remaining body keys forming the filter as
  before. (#33)
- `Options.ConnectTimeout`, `ShutdownTimeout`, and `ReadHeaderTimeout` (with
  `SetConnectTimeout`/`SetShutdownTimeout`/`SetReadHeaderTimeout` setters, defaulting
  to 10s/10s/5s) bound the initial Mongo connect/ping, graceful shutdown, and header
  reads respectively. (#29)
- `Options.MaxBodyBytes` (default 1 MiB, `0` disables) caps `/api` request body size
  via an `http.MaxBytesReader`-wrapping middleware; oversized bodies now return `413`
  instead of being read unbounded. (#35)
- `Options.AllowUnsafeOperators` / `SetAllowUnsafeOperators(true)` opts back into
  `$where`/`$function`/`$out`/`$merge` support in `find`/`count`/`aggregate`, which are
  now rejected by default (see Security). (#48)
- `SetAPIKey(apiKey string)`: a convenience wrapper around `SetAPIMiddleware` for a
  constant-time-compared `X-API-Key` header check. (#48)

### Changed

- **Breaking:** All `/api/...` route failure responses now return a JSON envelope,
  `{"error": "<message>"}`, instead of a plain-text body. The status code for each
  failure case is unchanged; only the response body format changed. Consumers that
  matched on the previous plain-text error body will need to switch to parsing the
  `error` field of the JSON body instead. (#31)
- **Breaking:** `Options.Address` now defaults to `"localhost:8080"` (loopback-only)
  instead of `":8080"` (all interfaces). Reaching the server off-host, including for
  Docker port publishing, now requires an explicit `SetAddress`. (#48)
- `Start()` now runs its own `http.Server` instead of `gin.Engine.Run`, and listens for
  `SIGINT`/`SIGTERM` via `signal.NotifyContext`. On shutdown it calls
  `http.Server.Shutdown(ctx)` followed by `mongoClient.Disconnect(ctx)`, so the normal
  `docker stop`/`kubectl delete pod` path now drains in-flight requests and disconnects
  from Mongo cleanly instead of the process being killed mid-flight. (#29)
- `SetAPIMiddleware`/`SetCustomMiddleware` called after `Start()` has registered routes
  now log a warning instead of silently no-oping, since a silent no-op on what's
  commonly an auth hook fails open. (#35)
- Internal: the duplicated DB-resolution block in `getCollections`, `collectionFind`,
  `collectionCount`, and `collectionAggregate` was extracted into a shared
  `(*server).resolveDB` helper. No behavior change. (#30)

### Fixed

- `find` and `count` no longer reject a request with an empty body; an empty body is
  now treated as an empty filter instead of a `400`. `aggregate` now binds the request
  body as JSON regardless of the `Content-Type` header, instead of only when
  `Content-Type: application/json` is set. (#32)
- `collectionAggregate` no longer appends an invalid trailing `$limit` stage when the
  caller's pipeline already ends in `$out`/`$merge` (Mongo requires those to be
  terminal). (#48)

### Security

- `find`/`count` filters and `aggregate` pipelines now reject `$where`, `$function`,
  `$out`, and `$merge` by default, checked recursively so a banned operator nested
  inside `$and`/`$or`/`$expr` or a pipeline stage is still caught. Opt out via
  `Options.AllowUnsafeOperators`. (#48)
- `Start()` now runs a best-effort check after connecting and logs a warning if the
  authenticated Mongo user holds write privileges (`insert`/`update`/`remove`), since
  this library is intended to expose Mongo read-only. Advisory only; never fails
  `Start()`. (#48)
