# Contributing

Thanks for considering a contribution to `gomongoapi`. This is a small library — the whole
public API is `option.go` and `server.go` — so most changes are small in scope too.

## Getting started

```sh
make build   # go build ./...
make vet     # go vet ./...
make lint    # golangci-lint run ./... (requires golangci-lint installed locally)
make test    # go vet, then go test ./...
```

`make test` runs the full suite, including container-backed tests that spin up a real
MongoDB via [testcontainers-go](https://github.com/testcontainers/testcontainers-go). Those
tests skip gracefully (`t.Skipf`) if Docker isn't available, so `make test` still passes
without it — but if you can, run with Docker so you actually exercise the Mongo-backed
paths you're changing.

To run a single test:

```sh
go test ./... -run TestCollectionFind_Success -v
```

## Before opening a PR

- Run `make test` and `make vet`; CI runs the same, plus `golangci-lint` and a build of
  `examples/simpleDemo` (a separate Go module, so it isn't covered by `go build ./...` at
  the repo root).
- Keep changes scoped to the issue or bug at hand — avoid bundling unrelated refactors.
- Add tests for new behavior, following the existing naming and patterns:
  - `Test<Handler>_<Scenario>` naming, e.g. `TestCollectionFind_Success`,
    `TestCollectionFind_BadLimit`.
  - Request-validation-only paths (missing db name, bad limit, malformed body) get
    table-style unit tests that build a `*server` directly and invoke the handler with
    `gin.CreateTestContext`/`httptest` — no Mongo needed.
  - Anything that touches Mongo goes through the shared container in `TestMain`
    (`server_test.go`), skipping via `sharedMongoErr` when Docker isn't available.
- Update `CHANGELOG.md` under `## [Unreleased]` for any user-visible change (new option,
  route, behavior change, bug fix). Follow the existing [Keep a
  Changelog](https://keepachangelog.com/en/1.1.0/)-style sections (`Added`/`Changed`/
  `Fixed`/`Security`) and reference the PR/issue number.
- If you're changing documented behavior, update `README.md` and `CLAUDE.md` alongside the
  code — both are treated as living documentation, not just onboarding material.

## Design conventions

A few conventions worth knowing before changing handler code (see `CLAUDE.md` for the full
list):

- Every `/api/collections/...` route resolves its target database via the shared
  `(*server).resolveDB(ctx) (string, bool)` helper rather than duplicating the
  default-db/`database`-param logic per handler.
- `SetAPIMiddleware`/`SetCustomMiddleware`/`SetAPIKey` must be called before `Start()` —
  gin composes a route's handler chain at registration time, and routes are registered
  inside `Start()`.
- Unsafe operators (`$where`/`$function`/`$out`/`$merge`) are rejected by default in
  `find`/`count`/`aggregate` unless `Options.AllowUnsafeOperators` is set; new query paths
  that accept a caller-supplied filter or pipeline should run through the same check.

## Reporting issues

Open a GitHub issue with a clear description of the problem or proposal. For bugs, include
steps to reproduce and, if possible, a minimal example.
