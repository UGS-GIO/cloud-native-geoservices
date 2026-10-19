# geoparquet-featureserver

Serves GeoParquet files as ArcGIS FeatureServer layers. ArcGIS Online Map Viewer, Experience
Builder, StoryMaps and ArcGIS Pro can add them by URL. It is one Go binary. It reads the files in
place, from local disk or with HTTP range requests, and has no database and no data copy.

Each request reads only the row groups, pages and columns it needs. Row groups are pruned on the
statistics of the GeoParquet bbox covering columns, and inside a row group the bbox columns pick
the rows.

## Quickstart

A folder of GeoParquet files, one layer per file:

```bash
LAYERS_DIR=./data go run .
docker build -t geoparquet-featureserver .
docker run -p 8080:8080 -v "$PWD/data:/data:ro" -e LAYERS_DIR=/data geoparquet-featureserver
```

Or install a release, a tagged version cut on every merge to main:

```bash
go install github.com/UGS-GIO/geoparquet-featureserver@latest
```

A STAC ItemCollection (`items.json`), local or over https:

```bash
STAC_COLLECTION_URL=https://example.com/stac/items.json go run .
```

Then open `http://localhost:8080/`. It lists each layer and its URL:
`/ugs/rest/services/{id}/{render}/FeatureServer/0`. A layer with no renders has the render
`default`. Map Viewer does not accept an http URL in "Add layer from URL", but it accepts one in
the page URL:
`https://www.arcgis.com/apps/mapviewer/index.html?url=http://localhost:8080/ugs/rest/services/{id}/default/FeatureServer/0`.

## Layer sources

Set one of these. If more than one is set, the first in this list is used.

| Var | Source |
|---|---|
| `STAC_COLLECTION_URL` | STAC ItemCollection, URL or local file |
| `LAYERS_CONFIG` | JSON config file, URL or local file |
| `LAYERS_DIR` | local folder; each `*.parquet` file is a layer, with its file name as the id |

The source is read again every `CACHE_TTL` seconds, so you can add a layer without a restart. The
geometry type, extent and fields come from each file's GeoParquet metadata and schema.

**STAC.** Each item that has a `data` asset is a layer. The item `id` is the layer id, and
`properties.title` and `properties.description` are used when they are set. These optional
fields are also read:

- `properties["ugs:renders"]`: an object of named renders, `{"<name>": {"title": "…", "esri_url": "…"}}`.
  Each render is a separate FeatureServer.
- Assets whose key starts with `overview` and that have `ugs:simplify_tolerance` (degrees): a
  generalized copy of the data. A small-scale request reads the coarsest overview that is still
  finer than the request.

**Config.** URLs that are relative are resolved against the location of the config file.

```json
{"layers": [
  {"id": "faults", "title": "Faults", "description": "Quaternary faults", "url": "faults.parquet",
   "renders": [{"name": "by-age", "title": "Faults by age", "esri_url": "styles/faults-age.json"}]},
  {"id": "wells", "url": "https://example.com/data/wells.parquet"}
]}
```

`esri_url` is a JSON document with the keys `renderer` and, optionally, `labelingInfo`,
`minScale` and `maxScale`, as in a layer's `drawingInfo`. A render without `esri_url` uses the
default symbol.

**Folder.** Only local folders are supported. Object stores cannot be listed over plain https,
so for files on a CDN or in a bucket, use a config file or STAC.

## Data requirements

- GeoParquet 1.x with WKB geometry in EPSG:4326.
- An integer `feature_id` column. It is the object id. Queries by `objectIds` need it.
- Recommended: the bbox covering columns (`bbox_xmin`, `bbox_ymin`, `bbox_xmax`, `bbox_ymax`),
  rows sorted spatially (for example Hilbert order), small row groups, and a page index. Without
  the covering, a spatial filter reads the geometry of every row. With the page index, a small
  box reads only the bbox pages it meets, so small pages on those columns help.

## Environment

| Var | Default | What |
|---|---|---|
| `PORT` | 8080 | listen port |
| `STAC_COLLECTION_URL`, `LAYERS_CONFIG`, `LAYERS_DIR` | | layer source (see above) |
| `CACHE_TTL` | 300 | seconds: layer list and file-version refresh, and `Cache-Control` |
| `PUBLIC_URL` | request host | base of the URLs `/` lists |
| `DECODED_CACHE_MB` | 512 or 12.5% of memory | decoded column values |
| `SHAPE_CACHE_MB` | 256 or 6.25% | simplified geometry per zoom step |
| `PAGE_CACHE_MB` | 128 or 3% | fetched bytes |
| `RESPONSE_CACHE_MB` | 256 or 6.25% | encoded responses (kept compressed), by ETag |
| `GEOMETRY_BUDGET_MB` | 512 or 12.5% | compressed geometry read at once; past it, 503 |
| `COMPRESSION` | gzip | content codings offered, most preferred first: `gzip`, `br`, `zstd` |
| `MAX_RECORDS` / `TILE_MAX_RECORDS` | 2000 / 8000 | page size; tile page size |
| `MAX_GEOMETRY_ROWS` | 50000 | geometries one request reads before `where` |
| `MAX_WHERE_LENGTH` | 4096 | characters |
| `MAX_BODY_BYTES` | 1048576 | POST body |
| `MAX_OBJECT_IDS` | 10000 | ids in `objectIds` |
| `REQUEST_TIMEOUT_S` | 30 | per request; a timeout gives Esri error 504 |
| `MAX_SCANS` / `SCAN_WAIT_MS` | 2 / 2000 | full-layer scans at once, and the wait before a 503 |
| `BUDGET_WAIT_MS` | 5000 | wait for geometry budget or a heavy slot before a 503 |
| `MAX_WORK` / `WORK_WAIT_MS` | 4×CPUs / 2000 | queries that are not light, computing at once, and the wait before a 503 |
| `MAX_LIGHT` | 16×CPUs | light queries computing at once (cache hits are not counted) |
| `MAX_IN_FLIGHT` | 2048, or 1 per MiB of memory | open connections and requests at once |
| `HEAVY_MB` / `HEAVY_SLOTS` | 8 / CPUs−1 (at least 1) | a request that reads more geometry than this is heavy; this many run at once |
| `DATA_MIRROR` | | replace the host of data URLs with a local folder or another range server (benchmarks) |
| `PPROF_ADDR` | | for example `localhost:6060`, for `net/http/pprof` |
| `LOG_LEVEL` | warn | `debug` logs every range request |

Cache sizes default to the smaller of the listed size and a fraction of the container memory
(cgroup v2 `memory.max`). The Go heap gets a soft limit at 80% of it.

## Query support

`/query` accepts GET and POST. Supported parameters:

- `where`: SQL subset (comparisons, `AND`/`OR`/`NOT`, `IN`, `LIKE`, `IS NULL`, `BETWEEN`,
  `UPPER`/`LOWER`, `DATE` and `TIMESTAMP` literals), evaluated as on ArcGIS Online: field names
  match case-insensitively, strings compare and sort ignoring case, a literal converts to the
  type of the field it meets (`FIPS = '49035'`, `UPDATED > '2020-01-01'`), other type
  mismatches are a 400, and a comparison with a null is unknown, so `NOT` does not match it. A
  query needs a `where` unless it has `objectIds`, `geometry` or `outStatistics`.
- `objectIds`, `outFields`, `returnGeometry`, `orderByFields`, `resultOffset`,
  `resultRecordCount`, `resultType=tile`, `maxAllowableOffset`, `geometryPrecision`.
- `geometry` (envelope, point, multipoint, polyline, polygon), `geometryType`, `inSR`,
  `distance` + `units`, `spatialRel`: `Intersects`, `EnvelopeIntersects`, `Contains`, `Within`.
  As in ArcGIS Server, `Contains` and `Within` read "query geometry contains / is within the
  feature".
- `returnCountOnly`, `returnIdsOnly`, `returnExtentOnly`, `returnDistinctValues`.
- `outStatistics` (`count`, `sum`, `min`, `max`, `avg`, `stddev`, `var`) with
  `groupByFieldsForStatistics`.
- `f=json`, `f=geojson`, `f=pbf` (with `quantizationParameters`).

Responses carry an ETag (data version + normalized query) and `Cache-Control`, so a CDN in front
can serve repeated tiles.

Responses of 1 KB or more use the `COMPRESSION` coding that `Accept-Encoding` ranks highest (ties
go to the first listed), else gzip, else none, and each coding has its own ETag.

## Known gaps

- Point layers return multipoint geometry in `f=json` and `f=geojson`.
- `outSR` and `inSR`: only 4326, 4269 and Web Mercator (3857, 102100 and aliases). Other spatial
  references give a 400.
- `f=geojson` does not fully conform to RFC 7946 (for example, the multipoint geometry above).
- Statistics are not typed by their source field. For example, `min` of an integer field is
  reported as a double.
- Layer metadata reports `supportsCoordinatesQuantization: false` and
  `supportedQueryFormats: "JSON"`, though `f=pbf` and `quantizationParameters` work.
- `spatialRel` values other than the four above give a 400. There is no support for `having`,
  `returnZ`/`returnM`, attachments, related records or edits.
- The service path prefix `/ugs/rest/services` is fixed.
- `testdata/conformance/known_gaps.json` lists every other difference from ArcGIS Online that the
  conformance suite finds.
- Some behavior follows Koop, which this service replaced: `exceededTransferLimit` is null when
  the server did not page, and multipolygon rings are written hole first.

## Conformance

`conformance/` compares this server with real ArcGIS FeatureServer layers. Each reference layer
has a GeoParquet copy (object ids kept as `feature_id`, Esri field types, EPSG:4326 at full
precision) and Esri's recorded answers to the requests in `testdata/conformance/requests.json`.
The test serves the copy, sends the same requests and compares the answers. Row order counts only
where the request sets one, and numbers match within a tolerance. Every difference must be listed
in `known_gaps.json` with a reason, and a listed gap that no longer happens fails too.

- Small tier: three Utah layers (point, line, polygon) in `testdata/conformance/layers`. It runs
  in `go test ./...`.
- Large tier: two national layers. The data is a release asset, not in git. A nightly workflow
  (`conformance-large.yml`) downloads it and runs `TestLarge`, which also logs timings.

The layers and their request values are in `testdata/conformance/layers.json`. To refresh:

```bash
go run ./cmd/conformance-record                                  # small tier, in place
go run ./cmd/conformance-record -tier large -out /tmp/large      # large tier
CONFORMANCE_LARGE=/tmp/large go test ./conformance -run TestLarge -v -timeout 60m
tar -czf conformance-large.tar.gz -C /tmp/large .
gh release create conformance-data --title "Conformance data" --notes "Large conformance layers" --latest=false  # once
gh release upload conformance-data conformance-large.tar.gz --clobber
```

`-skip-extract` records the answers again and keeps the copies. `-names a,b` records only
those requests into the existing recordings.

## Development

```bash
go vet ./... && go test ./...                   # hermetic: fixtures in testdata/, no network
go test ./internal/where -fuzz FuzzParse        # also FuzzFromWKB (geom), FuzzRun (query)
```

| Package | Concern |
|---|---|
| `main` | env, memory sizing, wiring, graceful shutdown |
| `internal/server` | routes, params, limits and load shedding, ETag and response cache, errors |
| `internal/provider` | plan → select → read → shapes → features |
| `internal/plan` | what a request needs from the file |
| `internal/pq` | GeoParquet: footer, row groups, select, column decode, decoded cache |
| `internal/remote` | range reads, page cache, versions, retries |
| `internal/catalog` | layer sources, renders, renderer JSON, TTL cache with stale-on-error |
| `internal/query` | query semantics and response shapes |
| `internal/where` | SQL `where` parser and evaluator |
| `internal/geom` | WKB → simplified shape, shape cache, Mercator, Esri JSON, spatial tests |
| `internal/esri` | service, layer and field descriptions |
| `internal/pbf` | Esri FeatureCollection protocol buffer |
| `internal/js` | JavaScript conversion and comparison rules |
| `internal/jsonx`, `internal/lru` | ordered JSON writer; byte-bounded LRU |
| `conformance`, `cmd/conformance-record` | comparison with recorded Esri answers; the recorder |

## License

MIT
