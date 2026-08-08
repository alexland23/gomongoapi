# Changelog

All notable changes to this project are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed

- **Breaking:** All `/api/...` route failure responses now return a JSON envelope,
  `{"error": "<message>"}`, instead of a plain-text body. The status code for each
  failure case is unchanged; only the response body format changed. Consumers that
  matched on the previous plain-text error body will need to switch to parsing the
  `error` field of the JSON body instead. Targeted for the `v1.1.0` release. (#31)
