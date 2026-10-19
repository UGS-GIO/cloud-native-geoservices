# Configuration

The server reads all of its settings from environment variables. An empty variable is the same as
an unset one. A number that does not parse is ignored, and the default applies.

## Layer source

| Variable | Default | Meaning |
|---|---|---|
| `STAC_COLLECTION_URL` | | STAC ItemCollection, URL or local file |
| `LAYERS_CONFIG` | | JSON config file, URL or local file |
| `LAYERS_DIR` | | Local folder of `*.parquet` files |

Set one. See [Layer sources](layer-sources.md).

## Server

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `PUBLIC_URL` | request host | Base of the layer URLs that `/` lists, for example `https://features.example.com`. Without it, the server uses the request's `Host` header and `X-Forwarded-Proto`. |
| `CACHE_TTL` | `300` | Seconds. How often the server reads the layer source again and checks each file version. Also the `max-age` and `stale-while-revalidate` of `Cache-Control`. |
| `COMPRESSION` | `gzip` | Content codings that the server offers, most preferred first. A comma list of `gzip`, `br` and `zstd`. An unknown name stops the server at start. See [Compression](performance.md#compression). |
| `LOG_LEVEL` | `warn` | `debug`, `info`, `warn` or `error`. `debug` logs every range request. An unknown name stays at `warn`. |
| `PPROF_ADDR` | | Address for the Go `net/http/pprof` handlers, for example `localhost:6060`. Off when unset. |
| `DATA_MIRROR` | | Replaces the scheme and host of each data URL with a local folder or another range server that holds the same paths. For benchmarks without the CDN. |

## Caches

Each cache size is in MiB. The default is the smaller of the listed size and a fraction of the
container memory limit. See [Memory sizing](performance.md#memory-sizing).

| Variable | Default | Holds |
|---|---|---|
| `PAGE_CACHE_MB` | 128, or 3% of memory | Bytes fetched from data files |
| `DECODED_CACHE_MB` | 512, or 10% | Decoded column values, by file, column and row group or page |
| `SHAPE_CACHE_MB` | 512, or 15% | Simplified geometry, by file and zoom step |
| `RESPONSE_CACHE_MB` | 256, or 6.25% | Encoded responses, kept compressed, by ETag |

## Request limits

| Variable | Default | Meaning |
|---|---|---|
| `MAX_RECORDS` | `2000` | Most features in one page, with or without geometry. A larger `resultRecordCount` gets this many. The layer's `maxRecordCount`. |
| `TILE_MAX_RECORDS` | `8000` | Most features in one page for `resultType=tile`. The layer's `tileMaxRecordCount`. |
| `MAX_GEOMETRY_ROWS` | `50000` | Rows with geometry that one request reads before `where`. Past it, the response sets `exceededTransferLimit`. |
| `MAX_RESPONSE_MB` | `30` | Size of a features response before the page ends early with `exceededTransferLimit`. Cloud Run refuses responses over 32 MiB. `0` turns it off. |
| `MAX_WHERE_LENGTH` | `4096` | Characters in `where` |
| `MAX_BODY_BYTES` | `1048576` | Bytes in a POST body |
| `MAX_OBJECT_IDS` | `10000` | Ids in `objectIds` |
| `REQUEST_TIMEOUT_S` | `30` | Seconds per request. A timeout gives Esri error code 504. |

A request over a limit gets an Esri error with code 400. Counts, ids, extents and statistics are
not capped at `MAX_RECORDS`. See [Paging](querying.md#paging).

## Load shedding

These limits protect the server under load. A request that cannot get a slot in time gets HTTP
503 with `Retry-After`. See [Admission and 503](performance.md#admission-and-503).

| Variable | Default | Meaning |
|---|---|---|
| `MAX_IN_FLIGHT` | 2048, or 1 per MiB of memory | Open connections and requests at once. Past it, a request gets 503 at once, and new connections wait in the kernel backlog. |
| `MAX_SCANS` | `2` | Full-layer scans at once: `where`, statistics, distinct values or a non-id order, with no spatial filter or ids |
| `SCAN_WAIT_MS` | `2000` | Wait for a scan slot before 503 |
| `MAX_WORK` | 4 × CPUs | Queries that are not light, computing at once |
| `WORK_WAIT_MS` | `2000` | Wait for a work or light slot before 503 |
| `MAX_LIGHT` | 16 × CPUs | Light queries computing at once. Cache hits do not count. |
| `GEOMETRY_BUDGET_MB` | 512, or 12.5% of memory | Compressed geometry being read at once, across requests |
| `BUDGET_WAIT_MS` | `5000` | Wait for geometry budget or a heavy slot before 503 |
| `HEAVY_MB` | `8` | A request that reads more compressed geometry than this is heavy |
| `HEAVY_SLOTS` | CPUs − 1, at least 1 | Heavy requests that decode geometry at once |

CPUs is `GOMAXPROCS`, which is the number of CPUs available to the process.

## Go runtime

When the server finds a cgroup v2 memory limit, it sets the Go soft memory limit to 80% of it,
unless `GOMEMLIMIT` is set. With 2 GiB of memory or more, it also sets `GOGC` to 200, unless
`GOGC` is set. Set either variable to use your own value.
