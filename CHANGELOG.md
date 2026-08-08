# Changelog

All notable changes to this project are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `find` now supports `sort` and `skip` url params, and a `projection` field in the
  request body, mapping to `options.Find().SetSort`/`SetSkip`/`SetProjection`
  respectively. `sort` is a JSON object url-encoded into the query string (e.g.
  `?sort={"createdAt":-1}`); `projection` is a reserved top-level key in the request
  body, matched case-insensitively, with the remaining body keys forming the filter as
  before. (#33)

### Fixed

- `find` and `count` no longer reject a request with an empty body; an empty body is
  now treated as an empty filter instead of a `400`. `aggregate` now binds the request
  body as JSON regardless of the `Content-Type` header, instead of only when
  `Content-Type: application/json` is set. (#32)

### Changed

- **Breaking:** All `/api/...` route failure responses now return a JSON envelope,
  `{"error": "<message>"}`, instead of a plain-text body. The status code for each
  failure case is unchanged; only the response body format changed. Consumers that
  matched on the previous plain-text error body will need to switch to parsing the
  `error` field of the JSON body instead. Targeted for the `v1.1.0` release. (#31)
