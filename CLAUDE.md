# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`gomongoapi` (module `github.com/alexland23/gomongoapi`) is a small Go library, not an application. It stands up an HTTP server (via [gin](https://github.com/gin-gonic/gin)) that exposes a MongoDB instance over a fixed set of REST routes, primarily so Grafana's JSON API / Infinity plugins can query Mongo without custom glue. Consumers `go get` the package, build `*Options` via `ServerOptions()`, then call `NewServer(opts)` and `server.Start()`.

The whole public API is two files:
- `option.go` — the `Options` struct and its setters, returned with defaults by `ServerOptions()`.
- `server.go` — the `Server` interface, the unexported `server` struct implementing it, and all route handlers.

There is no `main.go` at the module root; `examples/simpleDemo` is a runnable example app (its own Go module) showing usage with `docker-compose` (Mongo + Grafana provisioning).

## Commands

Use the `Makefile` targets rather than raw `go` commands where possible:

```sh
make build        # go build ./...
make vet           # go vet ./...
make lint          # golangci-lint run ./... (requires golangci-lint installed locally)
make test          # go vet, then go test ./... (Mongo-backed tests skip if Docker is unavailable)
make test-verbose  # same, with -v
make cover         # test with coverage, print per-function coverage
make cover-html    # cover + open HTML coverage report
make tidy          # go mod tidy
make clean         # remove generated coverage files
```

To run a single test: `go test ./... -run TestCollectionFind_Success -v` (standard Go test filtering; there's only one package at the module root, so `./...` vs `.` doesn't matter here).

## Testing architecture

Tests rely on [testcontainers-go's mongodb module](https://github.com/testcontainers/testcontainers-go) to spin up a real MongoDB instance, not a mock. See `server_test.go`:

- `TestMain` starts one shared MongoDB container for the whole test binary (`sharedMongoErr` holds the error if startup fails, e.g. no Docker).
- Any test that needs a live Mongo connection should follow the existing pattern of skipping via `t.Skipf(...)` when `sharedMongoErr != nil`, so the suite still passes in environments without Docker (see the helper around server_test.go:76-81).
- Route handler tests build a `*server` directly and invoke handlers with `gin.CreateTestContext`/`httptest`, rather than spinning up the full HTTP stack, for the request-validation-only paths (missing db name, bad limit, bad body, etc.). Tests that need real query results go through the shared Mongo container.

When adding a new route or option, add both the "bad input" table-style unit tests (no Mongo needed) and, if the handler touches Mongo, a container-backed success/error-path test consistent with the existing naming (`Test<Handler>_<Scenario>`).

## Design points worth knowing before changing behavior

- **DB resolution**: every `/api/collections/...` route resolves the target database the same way — if `Options.DefaultDB` is set, use it; otherwise require a `database` query param and 400 if missing. This is factored into `(*server).resolveDB(ctx) (string, bool)`, which writes the 400 itself and returns `ok=false` on failure; handlers just `return` when `ok` is false. New routes needing DB resolution should call this helper rather than reintroducing the duplicated block.
- **Custom routes**: `/custom` (name configurable via `CustomRouteName`, default `"custom"`) and `/api` are separate gin router groups created in `NewServer`. `SetCustomRouteName` rejects `/` and `/api` (`ErrInvalidCustomRouteName`). Middleware for either group is added via `SetAPIMiddleware`/`SetCustomMiddleware`, applied at server-construction time, not route-registration time.
- **Middleware ordering constraint**: gin composes a route's handler chain at *registration* time, and `/api`/`/custom` routes are registered by `createRoutes()` inside `Start()`. So `SetAPIMiddleware`/`SetCustomMiddleware` only affect routes registered afterward — they must be called before `Start()`. Calling either one after `Start()` has run `createRoutes()` (tracked via `server.routesRegistered`, guarded by `server.mu`) logs a warning instead of silently doing nothing, since this is commonly used as an auth hook and a silent no-op there fails open.
- **find limit**: `FindLimit` is the default page size when no `limit` query param is given; `FindMaxLimit` (0 = unlimited) is an upper bound requests are rejected against. `FindLimit` is converted to a string once in `NewServer` (the `findLimit` field) for use as a gin default query value; `FindMaxLimit` is kept as an int (`maxLimit`).
- **aggregate pipeline**: the request body's `Aggregate` field is expected to be a JSON array; if absent, an empty pipeline is used (not an error). If present but not an array, it's a 400.
- **Request body size limit**: `Options.MaxBodyBytes` (default 1 MiB, 0 disables) is enforced by wrapping `c.Request.Body` in `http.MaxBytesReader` via middleware registered on the `/api` group in `NewServer` — before any caller-added middleware or `createRoutes()`, so it applies regardless of what's added later. A body exceeding the limit surfaces as `*http.MaxBytesError` from `bindJSONBody`/`ShouldBindJSON`; `writeBindError` maps that to `413`, and any other bind error to `400`.
- **Address default**: `Options.Address` defaults to `"localhost:8080"` (loopback-only), not a wildcard address — a consumer must explicitly `SetAddress` a non-loopback address (e.g. `:8080`) to be reachable off the host, including for Docker port publishing (see `examples/simpleDemo/main.go`, which sets this explicitly). See the README Security section for the rationale.
- **Unsafe operator filtering**: unless `Options.AllowUnsafeOperators` is true (default false), `collectionFind`/`collectionCount`/`collectionAggregate` reject `$where`/`$function`/`$out`/`$merge` with 400 via `findBannedOperator`, which recursively walks the decoded filter/pipeline (maps and slices) so a banned key nested inside `$and`/`$or`/`$expr` or a pipeline stage still gets caught. `endsInOutOrMerge` skips appending `collectionAggregate`'s trailing `$limit` stage when the caller's pipeline already ends in `$out`/`$merge` (only reachable with `AllowUnsafeOperators` true), since Mongo requires those to be the terminal stage.
- **Read-only credential warning**: `Start()` calls `warnIfWritableCredentials` after a successful Ping, which runs the `connectionStatus` admin command and logs (via `hasWriteAction`) if the authenticated user holds `insert`/`update`/`remove`. This is best-effort and non-fatal — any error running the command (no auth configured, `connectionStatus` disallowed by a custom role, etc.) is silently ignored rather than failing `Start()`.
- **`SetAPIKey`**: a thin convenience wrapper around `SetAPIMiddleware` that checks a constant-time-compared `X-API-Key` header; same before-`Start()` ordering constraint as `SetAPIMiddleware`/`SetCustomMiddleware`.
- `Server.Start()` blocks until the underlying `gin.Engine.Run` returns an error; it also performs the Mongo `Connect`/`Ping` and defers `Disconnect`. It returns an error rather than panicking if `Options.Router` is nil or the Mongo connection/ping fails. `NewServer` itself also tolerates a nil `Options.Router` — it skips creating the route groups rather than panicking, leaving `Start()`'s nil check as the single place that surfaces the error.
