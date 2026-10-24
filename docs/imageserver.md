# ImageServer

`imageserver/` serves Cloud Optimized GeoTIFFs as ArcGIS ImageServers, so ArcGIS Online Map
Viewer, StoryMaps, Experience Builder and Field Maps can add a COG by URL:
`https://.../ugs/rest/services/{id}/ImageServer`. It is a Python service (FastAPI) built on
Development Seed's [titiler](https://github.com/developmentseed/titiler) and
[rio-tiler](https://github.com/cogeotiff/rio-tiler). It reads the COGs in place with HTTP range
requests and renders each request on demand. There is no tile cache to build; the CDN caches the
responses.

## What clients ask for

The ArcGIS Maps SDK for JavaScript (4.33, which Map Viewer, StoryMaps and Experience Builder use)
picks the layer type from the service description:

- With `tileInfo`, it makes an `ImageryTileLayer`. With `cacheType: "Map"` and a JPEG, PNG or
  Mixed tile format, it reads `tile/{level}/{row}/{col}` and nothing else. This server advertises
  a Web Mercator tile cache, so clients draw from tiles, which the CDN can cache.
- Without `tileInfo`, it makes an `ImageryLayer`. That layer reads `keyProperties`, asks for
  `rasterAttributeTable`, `colormap`, `histograms` and `multidimensionalInfo` only when the
  service says it has them, asks for `rasterFunctionInfos` unless the only function is `None`,
  and draws with one `exportImage` per view (`f=image`, `format=jpgpng`, `bbox`, `bboxSR` and
  `imageSR` in the view's spatial reference, `size` in pixels).
- Popups use `identify` with a point.

So the endpoints are:

| Endpoint | rio-tiler call | Notes |
|---|---|---|
| `{id}/ImageServer?f=json` | `Reader.info()` | Web Mercator extent; tiles to the zoom level of the COG's pixel size, rounded up |
| `{id}/ImageServer/tile/{level}/{row}/{col}` | `STACReader.tile()` | 256 px, JPEG when opaque, else PNG (`Mixed`); 404 outside the raster |
| `{id}/ImageServer/exportImage` | `STACReader.part()` | `png`, `jpg`, `jpgpng`; any wkid or WKT for `bboxSR` and `imageSR`; `RSP_NearestNeighbor`, `RSP_BilinearInterpolation` (default), `RSP_CubicConvolution`; transparent PNG where there is no data |
| `{id}/ImageServer/identify` | `STACReader.point()` | `value` is the bands joined with `", "`, or `NoData` |
| `{id}/ImageServer/keyProperties`, `rasterFunctionInfos` | | static |
| `{id}/ImageServer/legend`, `statistics`, `histograms`, `rasterAttributeTable` | | empty, as ArcGIS Server answers for a raster without them |
| `{id}/ImageServer/{any other}` | | the service description, as on ArcGIS Server |

`/` lists each service and its URL. `/ugs/rest/services?f=json` lists them as ArcGIS Server does.
As on ArcGIS Server, errors are a JSON body with HTTP 200, and `exportImage` grows the `bbox`
about its center to the aspect ratio of `size` unless `adjustAspectRatio=false`. `exportImage`
and `identify` also take their parameters as a POST form.

Each answer has an `ETag`, a hash of the build (`GIT_SHA`, else a hash of the package's files and
the titiler, rio-tiler and rasterio versions), the STAC item, the COG's own `ETag` (or
`Last-Modified` and size, from a HEAD request every `CACHE_TTL`; a failed HEAD keeps the last one)
and the request. A COG rewritten in place or a new build changes it. `If-None-Match` is checked
before the COG is read.

## Layer sources

`STAC_COLLECTION_URL` is one or more STAC ItemCollections (`items.json`), comma separated. Each
item that has a GeoTIFF asset under one of `ASSET_KEYS` (default `visual,cog,data`, first match
wins) is a service, with the item `id` as its id. The list is read again every `CACHE_TTL`
seconds in the background.

The UGS warehouse COGs are:

| Collection | Asset | Layout |
|---|---|---|
| `ugs-rasters/geolmap_24k_series` | `visual` (EPSG:3857); `cog` is the native UTM copy, with a rotated geotransform | RGB, 8 bit, DEFLATE, 512 px tiles, 5 overviews, no nodata |
| `ugs-publications/*` (`geolmap/cogs/`) | `cog` (EPSG:3857, pixel sizes of zoom 16 or 17) | RGBA (alpha collar), 8 bit, mostly lossy WebP, some DEFLATE, 512 px tiles, 4 to 6 overviews |

The rasterio wheel's GDAL reads all of these, WebP included.

Only 8-bit rasters are drawn. Other pixel types give an error until there is a rule for how to
show them (a stretch or a color map).

## Configuration

| Var | Default | What |
|---|---|---|
| `STAC_COLLECTION_URL` | | ItemCollections, comma separated, URLs or local files |
| `ASSET_KEYS` | `visual,cog,data` | asset keys to serve, in order of preference |
| `CACHE_TTL` | 300 | seconds: catalog refresh and `Cache-Control` `max-age` |
| `CDN_TTL` | `CACHE_TTL` | seconds a shared cache (CDN) keeps a response: `s-maxage` |
| `MAX_IMAGE_SIZE` | 2048 | largest `exportImage` width or height |
| `MAX_EXPORTS` | 1 | `exportImage` reads at once; the others wait. A 2048 × 2048 export takes about 80 MB |
| `EXPORT_WAIT` | 10 | seconds an `exportImage` waits for a slot before it answers error 503 (busy) |
| `GIT_SHA` | | the commit, for the ETags; the Dockerfile sets it from `--build-arg GIT_SHA` |
| `PUBLIC_URL` | request host | base of the URLs `/` lists |
| `PORT` | 8080 | in the container |

GDAL settings, set in `imageserver/__init__.py` unless the environment sets them:
`GDAL_DISABLE_READDIR_ON_OPEN=EMPTY_DIR` (no directory listing), `GDAL_HTTP_MULTIPLEX=YES` and
`GDAL_HTTP_VERSION=2` (one connection for many ranges), `GDAL_HTTP_MERGE_CONSECUTIVE_RANGES=YES`,
`GDAL_INGESTED_BYTES_AT_OPEN=32768` (the header in one request), `CPL_VSIL_CURL_CACHE_SIZE` 64 MB,
`VSI_CACHE`, `GDAL_CACHEMAX=128` MB, and `RIO_TILER_MAX_THREADS=1`. The last keeps each read in the
request's thread: GDAL keeps HTTP connections per thread, and a new thread per request paid a new
TLS handshake every time.

Every read goes through a warped VRT (`warp_mem_limit` 64 MB). Without it, a read in the COG's own
CRS that runs past its edge is a boundless read, which skips the overviews: a full-extent
1200 × 1600 export of a WebP COG took 56 s instead of 2 s.

## Running

```bash
cd imageserver
uv sync --extra dev
STAC_COLLECTION_URL=https://maps-assets.geology.utah.gov/warehouse/stac/ugs-rasters/geolmap_24k_series/items.json \
  uv run uvicorn --factory imageserver.main:create_app --port 8080
uv run pytest -q
```

From the repository root:

```bash
docker build -f imageserver/Dockerfile -t imageserver .
docker run -p 8080:8080 -e STAC_COLLECTION_URL=https://example.com/stac/items.json imageserver
```

The image is `python:3.12-slim` with the locked dependencies, about 380 MB. It answers about 1.3 s
after start and uses about 135 MB of memory after a few requests.

On Cloud Run, set `--concurrency` low, such as 8. Requests run on 40 threads, and exports that
wait for a slot hold threads that tiles need: 45 queued exports raised the slowest tile from 2.8 s
to 13.9 s. A low concurrency makes Cloud Run start another instance instead.

## Performance

Measured in the container (1 CPU, 512 MB) against the warehouse CDN, each request the first to
its part of the COG:

| Request | Time |
|---|---|
| service description, first for a COG (opens the header) | 0.5 s |
| `tile` at the COG's zoom levels | 0.07 to 0.1 s; 0.4 s for the first at a new overview |
| `exportImage` 512 × 512 at full resolution | 0.1 s |
| `exportImage` 1024 × 1024, mid zoom | 0.6 s |
| `exportImage` 1200 × 1600, whole map, PNG | 1.5 s, most of it PNG encoding |
| `exportImage` 800 × 800 in EPSG:4326 | 0.45 s |
| `exportImage` 2048 × 2048, PNG | 3 to 6 s; two at once take 3 and 5.4 s, as the second waits |
| `identify` | 0.4 s first, then a few ms |

Memory peaked at 337 MB over 48 exports of 2048 × 2048, 16 at a time. With `MAX_EXPORTS=2` it
reached 500 MB, and without `MALLOC_ARENA_MAX=2` the container ran out of memory.

## Conformance

`testdata/conformance/imageserver` holds ArcGIS Server's answers to the requests in
`requests.json`, from three reference services in `layers.json`: Esri's Toronto (a cached
mosaic dataset), Mt. Baldy elevation (a single raster in UTM) and USGS NAIP Plus. To record again:

```bash
cd imageserver && uv run python scripts/record.py      # -only toronto for one
```

The reference rasters are not copied, so `tests/test_conformance.py` checks what does not depend
on the pixels: that the service description has every key of both kinds of service, with the
same types, the tile scheme, the shape of `exportImage` and `identify` answers, the extent after
the aspect-ratio adjustment, errors, and content types.

## Known gaps

- `legend`, `statistics`, `histograms` and `rasterAttributeTable` are empty.
  `computeStatisticsHistograms`, `getSamples`, `query` and raster functions are not served. Map
  Viewer draws the legend of an RGB layer itself.
- `exportImage` with `f=json` returns an `href` to the same request with `f=image`, not to a
  temporary file.
- `bandIds`, `noData`, `pixelType`, `mosaicRule` and `renderingRule` are ignored.
- One `id` is one COG. There are no mosaics of several COGs.
