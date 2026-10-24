"""ArcGIS ImageServer over Cloud Optimized GeoTIFFs listed in STAC, on titiler and rio-tiler."""

import os

# GDAL reads each COG with range requests: no directory listing, one connection per host for many
# ranges, adjacent ranges merged, and a shared cache of the fetched chunks.
GDAL_DEFAULTS = {
    "GDAL_DISABLE_READDIR_ON_OPEN": "EMPTY_DIR",
    "CPL_VSIL_CURL_ALLOWED_EXTENSIONS": ".tif,.tiff,.TIF,.TIFF",
    "GDAL_HTTP_MULTIPLEX": "YES",
    "GDAL_HTTP_VERSION": "2",
    "GDAL_HTTP_MERGE_CONSECUTIVE_RANGES": "YES",
    "GDAL_INGESTED_BYTES_AT_OPEN": "32768",
    "CPL_VSIL_CURL_CACHE_SIZE": str(64 << 20),
    "VSI_CACHE": "TRUE",
    "VSI_CACHE_SIZE": str(8 << 20),
    "GDAL_CACHEMAX": "128",
    "GDAL_BAND_BLOCK_CACHE": "HASHSET",
    "PROJ_NETWORK": "OFF",
    # One asset per request: read it in the request's thread, whose HTTP connections stay open.
    "RIO_TILER_MAX_THREADS": "1",
}
for k, v in GDAL_DEFAULTS.items():
    os.environ.setdefault(k, v)
