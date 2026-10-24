# Contributing and releases

## Build and test

```bash
go vet ./... && go test ./...                   # hermetic: fixtures in testdata/, no network
go test ./internal/where -fuzz FuzzParse        # also FuzzFromWKB (geom), FuzzRun (query)
```

CI runs `gofmt`, `go vet`, `go test` and `go build` on every pull request and on `main`.
`go test ./...` includes the small tier of the [conformance suite](conformance.md). The large tier
runs nightly.

To preview these docs:

```bash
pip install -r docs/requirements.txt
mkdocs serve
```

## Code layout

| Package | Concern |
|---|---|
| `main` | Environment, memory sizing, wiring, graceful shutdown |
| `internal/server` | Routes, parameters, limits and load shedding, ETag and response cache, errors |
| `internal/provider` | Plan, select, read, shapes, features |
| `internal/plan` | What a request needs from the file |
| `internal/pq` | GeoParquet: footer, row groups, select, column decode, decoded cache |
| `internal/remote` | Range reads, page cache, versions, retries |
| `internal/catalog` | Layer sources, renders, renderer JSON, TTL cache with stale-on-error |
| `internal/query` | Query semantics and response shapes |
| `internal/where` | SQL `where` parser and evaluator |
| `internal/geom` | WKB to simplified shape, shape cache, Mercator and NAD83, Esri JSON, spatial tests, distance, quantization |
| `internal/esri` | Service, layer and field descriptions |
| `internal/pbf` | Esri FeatureCollection protocol buffer |
| `internal/js` | JavaScript number and string conversions, for parameters and PBF |
| `internal/jsonx`, `internal/lru` | Ordered JSON writer, byte-bounded LRU |
| `conformance`, `cmd/conformance-record` | Comparison with recorded ArcGIS Online answers, and the recorder |

## Pull requests

Pull requests are squash merged, so the PR title becomes the commit message on `main`. The
`pr-title` check requires a [conventional commit](https://www.conventionalcommits.org/) title:

- The type is one of `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`, `chore`
  or `revert`. A scope is optional, for example `fix(query): ...`.
- The subject starts with a lowercase letter.
- The title is at most 100 characters.

## Releases

Every merge to `main` is a release. The `release` workflow runs
[go-semantic-release](https://github.com/go-semantic-release/semantic-release). It reads the
conventional commits since the last tag, tags the next version and writes the release notes.

While the version is below 1.0:

| Commit type | Version bump |
|---|---|
| `feat` | Minor, for example 0.2.0 to 0.3.0 |
| Any other type | Patch, for example 0.2.0 to 0.2.1 |

Users install a release with `go install github.com/UGS-GIO/cloud-native-geoservices/cmd/featureserver@latest`, or
with `@vX.Y.Z` for a fixed version.

## Documentation

This site is built from `docs/` with MkDocs Material. The `docs` workflow builds it with
`mkdocs build --strict` on each pull request and publishes it to GitHub Pages on each push to
`main`. Change the docs in the same pull request as the code that they describe.
