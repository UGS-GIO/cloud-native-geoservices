# geoservices: an ArcGIS FeatureServer for Map Viewer, Experience Builder and StoryMaps

ArcGIS Online draws OGC API Features (`featureserv/`) in one default symbol. A FeatureServer
carries its own renderer, so this service gives AGOL the same symbology the web viewer draws, with
popups and attribute tables. It is one Go binary that reads the warehouse GeoParquet on the CDN
with https range requests. There is no data copy, and it scales to zero.

## Routes

| Route | Gives you |
|---|---|
| `/` | every topic and render, with the URL to add |
| `/ugs/rest/services/{topic}/{render}/FeatureServer/0` | one layer per ugs-styles render (`default`, `by-purpose`, …) |

In Map Viewer: **Add layer → Add layer from URL**, paste the `FeatureServer/0` URL. A topic with
two renders (UCRC wells: `by-purpose`, `by-boxtype`) is two layers to add.

- **Layers** come from the serving-topics `items.json` (`STAC_COLLECTION_URL`), cached for
  `CACHE_TTL` seconds. A new topic or a re-ingest shows up within that time, with no redeploy.
- **Symbology** is the Esri renderer ugs-styles builds next to each GL style (`build:esri` there),
  passed through as `ugs:renders[render].esri_url`. A render without one gets the default symbol.
- **Responses** match Koop's, which this service replaced: same routes, fields, Esri JSON and PBF.

## Run

```bash
go run .                                        # PORT=8080
PORT=8096 STAC_COLLECTION_URL=./items.json go run .
go test ./...                                   # hermetic: fixtures in testdata/, no network
go test ./internal/where -fuzz FuzzParse        # also FuzzFromWKB (geom), FuzzRun (query)
docker build -t geoservices .                   # static binary on distroless
# Map Viewer refuses http URLs in "Add layer", but takes one in the page URL:
# https://www.arcgis.com/apps/mapviewer/index.html?url=http://localhost:8096/ugs/rest/services/hazards_qfaults/default/FeatureServer/0
```

`STAC_COLLECTION_URL` can be a local file, to test renderers before ugs-styles publishes them.

## Environment

| Var | Default | What |
|---|---|---|
| `PORT` | 8080 | listen port |
| `STAC_COLLECTION_URL` | serving-topics `items.json` on the CDN | URL or local file |
| `CACHE_TTL` | 300 | seconds: catalog and file-version refresh, and `Cache-Control` |
| `PUBLIC_URL` | request host | base of the URLs `/` lists |
| `DECODED_CACHE_MB` | 512 or 12.5% of memory | decoded column values |
| `SHAPE_CACHE_MB` | 256 or 6.25% | simplified geometry per zoom step |
| `PAGE_CACHE_MB` | 128 or 3% | fetched bytes |
| `RESPONSE_CACHE_MB` | 256 or 6.25% | encoded responses (kept gzipped), by ETag |
| `GEOMETRY_BUDGET_MB` | 512 or 12.5% | compressed geometry being read at once; past it, 503 |
| `MAX_RECORDS` / `TILE_MAX_RECORDS` | 2000 / 8000 | page size; tile page size |
| `MAX_GEOMETRY_ROWS` | 50000 | geometries one request reads before `where` |
| `MAX_WHERE_LENGTH` | 4096 | characters |
| `MAX_BODY_BYTES` | 1048576 | POST body |
| `MAX_OBJECT_IDS` | 10000 | ids in `objectIds` |
| `REQUEST_TIMEOUT_S` | 30 | per request; a timeout answers Esri error 504 |
| `MAX_SCANS` / `SCAN_WAIT_MS` | 2 / 2000 | full-layer scans (where/statistics/distinct with no bbox or page) at once, and how long one waits before a 503 |
| `BUDGET_WAIT_MS` | 5000 | how long a request waits for geometry budget (or a heavy slot) before a 503 |
| `MAX_WORK` / `WORK_WAIT_MS` | 4×CPUs / 2000 | queries that are not light (deep tile, identify, few ids, footer count) computing at once; past the wait, 503 + `Retry-After` |
| `MAX_LIGHT` | 16×CPUs | light queries computing at once (cache hits skip this); past `WORK_WAIT_MS`, 503 |
| `MAX_IN_FLIGHT` | 2048, or 1 per MiB of memory | open connections, and requests at once (past it, an immediate 503) |
| `HEAVY_MB` / `HEAVY_SLOTS` | 8 / CPUs−1 (at least 1) | a request estimated to read more geometry than this is heavy; this many run at once |
| `DATA_MIRROR` | | replace the CDN host of data URLs with a local directory or another range server (benchmarks) |
| `PPROF_ADDR` | | e.g. `localhost:6060` for `net/http/pprof` |
| `LOG_LEVEL` | warn | debug logs every range request |

Cache budgets default to the smaller of the listed size and a fraction of the container memory
(`memory.max` of the process's cgroup), and the Go heap gets a soft limit at 80% of it, so the same
binary fits 1 GiB and 4 GiB.

## How a request flows

1. `server`: limits, deadline, ETag (data version + normalized query). A hit in the
   encoded-response cache or an `If-None-Match` match answers without reading anything; the cache
   keeps large responses gzipped, so a hit is never encoded or compressed again. Queries that are
   not light (deep tiles, identify, a few ids) wait for one of `MAX_WORK` slots, and geometry-heavy
   ones for one of `HEAVY_SLOTS`, so light requests do not queue behind statewide work.
2. `provider`: catalog topic, `plan` (columns, bbox prefilter, ids, page, tolerance), `pq.Select`
   prunes row groups on bbox statistics and filters rows on the bbox columns, then reads the plan's
   columns. Geometry comes from the shape cache; only misses read WKB, and those are simplified in
   parallel. Small scales read the archive's `overview` asset when the item has one. Specks are
   thinned, and a drawing tile over its cap keeps its largest features.
3. `query`: `where`, objectIds, exact intersects, order, distinct, statistics, paging, projection,
   Esri JSON or GeoJSON; `pbf` encodes `f=pbf`.

Reads: one HTTP/1.1 connection pool (a multiplexed HTTP/2 connection let one stalled stream hold up
the rest), ranges joined when close and split into parallel 4 MiB parts when large, retried with
jittered backoff, singleflight per range. A re-ingest overwrites the same URL, so every cache keys
on the file version (ETag, or Last-Modified and size), checked with one HEAD per file per
`CACHE_TTL`; range reads send `If-Match` so a mid-read re-ingest fails instead of mixing versions.
Archives with a page index (`vector/sink_archive.py`) are read a page at a time; older ones a whole
column chunk at a time.

## Packages

| Package | Concern |
|---|---|
| `main` | env, memory sizing, wiring, graceful shutdown |
| `internal/server` | routes, params, limits and load shedding, ETag and response cache, errors |
| `internal/provider` | plan → select → read → shapes → features |
| `internal/plan` | what a request needs from the file |
| `internal/pq` | GeoParquet: footer, row groups, select, column decode, decoded cache |
| `internal/remote` | https range reads, page cache, versions, retries |
| `internal/catalog` | STAC items, renders, renderer JSON, TTL cache with stale-on-error |
| `internal/query` | Koop/winnow query semantics and response shapes |
| `internal/where` | SQL `where` parser and evaluator (alasql's null and comparison rules) |
| `internal/geom` | WKB → simplified shape, shape cache, Mercator, Esri JSON, intersects |
| `internal/esri` | service, layer and field descriptions from Koop's defaults |
| `internal/pbf` | Esri FeatureCollection protocol buffer |
| `internal/js` | JavaScript conversion and comparison rules (Number, String, ==, ===, <) |
| `internal/jsonx`, `internal/lru` | JSON.stringify-compatible writer; byte-bounded LRU |

## Libraries, and what is hand-written

parquet-go (footer, pages, zstd via klauspost/compress), `net/http`, protowire, paulmach/orb
(geometry types, `wkb` in tests, `planar.RingContains`), x/sync (errgroup, singleflight,
semaphore). Hand-written, each because Koop's output depends on it:

- WKB → simplified shape: one pass over the bytes, Douglas-Peucker with Koop's speck rule; orb's
  `wkb` + `simplify` would build every point first and keep different points.
- Mercator: proj4's exact formulas; orb's `project` differs in the last bits, which moves quantized
  PBF coordinates across rounding boundaries.
- Intersects: turf's `booleanIntersects` (boundaries count, every ring counts); orb has no general
  intersects predicate.
- `where`: no Go SQL parser evaluates with alasql's JavaScript comparison rules.
- Esri JSON and PBF shapes: Koop's key order, ring order and quantization quirks.

Koop quirks kept on purpose, because clients see them: `where` containing `1=1` anywhere is ignored;
a null `exceededTransferLimit` when the provider did not page; multipolygon rings written hole
first; `outSR=900913` reported as 4326; `STDDEV` is the population and `VAR` the sample statistic.
Deliberate differences: `where` field names match case-insensitively, `DATE '…'` literals compare
as epoch milliseconds, spatial references other than WGS84 and Web Mercator are a 400, and
statistics with paging or `f=pbf` answer instead of erroring.

## Performance

On 2 CPU / 4 GiB it sustains over 1,280 Map Viewer requests a second with p99 under a second
(Koop: none at that p99), and sheds load with 503 + `Retry-After` past that instead of running
out of memory. On paged archives 1 CPU / 1 GiB sustains about 320. A cold request on a heavy
layer is bound by fetching its geometry from the CDN.
