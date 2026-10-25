# Layer sources

A layer source tells the server which layers to serve. Set one of these environment variables.
If you set more than one, the server uses the first in this list.

| Variable | Source |
|---|---|
| `STAC_COLLECTION_URL` | A STAC ItemCollection (`items.json`), as a URL or a local file |
| `LAYERS_CONFIG` | A JSON config file, as a URL or a local file |
| `LAYERS_DIR` | A local folder. Each `*.parquet` file is a layer. |

If you set none, the server logs an error and exits with code 2.

The server reads the source again every `CACHE_TTL` seconds (default 300), so you can add or
remove a layer without a restart. If a refresh fails, the server keeps the last good list and
tries again after 10 seconds.

The source gives only ids, titles, file locations and renders. The geometry type, extent and
fields come from each file's GeoParquet metadata and schema.

## Folder

```bash
LAYERS_DIR=/data
```

Each `*.parquet` file in the folder is a layer. The file name without `.parquet` is the layer id.
The server does not read subfolders.

Only local folders are supported. Object stores cannot be listed over plain HTTPS, so for files
on a CDN or in a bucket, use a config file or STAC.

## Config file

```bash
LAYERS_CONFIG=https://example.com/layers.json
```

```json
{"layers": [
  {"id": "faults", "title": "Faults", "description": "Quaternary faults", "url": "faults.parquet",
   "renders": [{"name": "by-age", "title": "Faults by age", "esri_url": "styles/faults-age.json"}]},
  {"id": "wells", "url": "https://example.com/data/wells.parquet"}
]}
```

| Key | Required | Meaning |
|---|---|---|
| `id` | yes | Layer id, used in the service URL |
| `url` | yes | Location of the GeoParquet file |
| `title` | no | Layer name. The default is the id. |
| `description` | no | Layer and service description |
| `renders` | no | List of [renders](#renders), each with `name`, `title` and `esri_url` |

A relative `url` or `esri_url` resolves against the location of the config file. If two layers
have the same id, the last one is used.

## STAC

```bash
STAC_COLLECTION_URL=https://example.com/stac/items.json
```

The file is a STAC ItemCollection: an object with an `items` array. Each item that has a `data`
asset is a layer. Items without a `data` asset are skipped. The server reads these fields:

| Field | Use |
|---|---|
| `id` | Layer id |
| `properties.title` | Layer name |
| `properties.description` | Layer and service description |
| `properties["ugs:renders"]` | Named [renders](#renders): `{"<name>": {"title": "…", "esri_url": "…"}}` |
| `assets.data.href` | The GeoParquet file |
| `assets.overview*.href` and `ugs:simplify_tolerance` | [Overviews](#overviews) |

The `ugs:` field names come from the project's first deployment and are fixed in the current
release.

```json
{"items": [
  {"id": "faults",
   "properties": {"title": "Faults",
     "ugs:renders": {"by-age": {"title": "Faults by age", "esri_url": "https://example.com/styles/faults-age.json"}}},
   "assets": {
     "data": {"href": "https://example.com/data/faults.parquet"},
     "overview_1": {"href": "https://example.com/data/faults_ov1.parquet", "ugs:simplify_tolerance": 0.001},
     "overview_2": {"href": "https://example.com/data/faults_ov2.parquet", "ugs:simplify_tolerance": 0.01}}}
]}
```

!!! note
    Asset hrefs and `esri_url` values in STAC are used as written. They do not resolve against the
    location of `items.json`, so use absolute URLs.

## Renders

A render is one way to draw a layer. Each render is a separate FeatureServer, at
`/ugs/rest/services/{id}/{render}/FeatureServer` (the prefix is fixed). A layer with no renders has one render,
`default`, which uses a simple default symbol for its geometry type.

`esri_url` points to a JSON document with the same keys as a layer's `drawingInfo`:

```json
{"renderer": {"type": "uniqueValue", "field1": "age", "uniqueValueInfos": []},
 "labelingInfo": [],
 "minScale": 0,
 "maxScale": 0}
```

All keys are optional. A missing `renderer`, or a render without `esri_url`, uses the default
symbol. The server fetches the document
when a request needs it, and keeps it for `CACHE_TTL` seconds. A change to the document changes
the [ETag](performance.md#response-cache-and-etags) of the render's responses.

In the service name, a render other than `default` adds ` (render)` to the layer title.

## Overviews

An overview is a generalized copy of the layer data, in a separate GeoParquet file. Only STAC
sources can declare overviews.

An asset is an overview when its key starts with `overview` and it has `ugs:simplify_tolerance`,
the simplification tolerance in degrees. For a small-scale feature request (one with
`maxAllowableOffset`), the server reads the coarsest overview whose tolerance is not larger than
the request's tolerance. If no overview is fine enough, it reads the full data.

The server does not use an overview for requests by `objectIds`, for counts, ids or statistics, or
for requests without geometry. An overview must have the same columns that requests ask for.

The layer description lists the overviews in `multiScaleGeometryInfo`, as ArcGIS Online does for
a layer with optimized drawing. Each level is the web map zoom level at which the overview's
tolerance is about one pixel, `round(log2(1.40625 / tolerance))`, so tolerances of 0.00068,
0.0027 and 0.0108 degrees give `{"levels": [7, 9, 11]}`.

## Data requirements

Each file must be GeoParquet 1.x or 2.0 with these properties:

- WKB geometry in EPSG:4326 (OGC:CRS84). The geometry column is the `primary_column` in the `geo`
  metadata. Without one, it is the first column of the Parquet `GEOMETRY` or `GEOGRAPHY` type, or
  else `geom`.
- An integer `feature_id` column. It is the object id. Queries by `objectIds` need it.
- Rows in `feature_id` order. The server pages by row number when there is no `where` filter, so
  a page of rows must be a page of features.

The geometry type comes from the first entry of `geometry_types` in the `geo` metadata. A type
that contains `Point` gives a point layer, a type that contains `LineString` gives a polyline
layer, and anything else gives a polygon layer. Without `geometry_types`, the type comes from the
geospatial statistics of the first row group. Each file is one geometry type.

These are recommended:

- The bbox covering as four flat columns: `bbox_xmin`, `bbox_ymin`, `bbox_xmax`, `bbox_ymax`.
  Without them, the layer extent is not known, and a spatial filter reads the geometry of every
  row in the row groups it meets. Row groups are then pruned on the geospatial statistics of a
  `GEOMETRY` or `GEOGRAPHY` column, if the file has them. A row group without them is always
  read, and a `GEOGRAPHY` box across the antimeridian prunes on latitude only.
- Rows sorted spatially, for example in Hilbert order, with `feature_id` assigned in that order.
- `feature_id` equal to the row number plus 1. Then the server finds an object id without reading
  the `feature_id` column. Otherwise it reads `feature_id` in the row groups whose statistics may
  hold the id, so ids in order keep that read small.
- Small row groups and a page index (the column index and offset index). Small pages on the bbox
  columns (about 100 rows) let a small box read only the pages it meets.

The columns `bbox`, `bbox_xmin`, `bbox_ymin`, `bbox_xmax`, `bbox_ymax` and `target_epsg` are not
served as fields.

### Field types

| Parquet column | Esri field type |
|---|---|
| `feature_id` | `esriFieldTypeOID` |
| `INT32`, `BOOLEAN` | `esriFieldTypeInteger` |
| `INT64` | `esriFieldTypeBigInteger` |
| Unsigned `INT64`, `FLOAT`, `DOUBLE` | `esriFieldTypeDouble` |
| `DATE`, `TIMESTAMP` (milli, micro or nano) | `esriFieldTypeDate` |
| Anything else | `esriFieldTypeString` |

Date values are milliseconds since the Unix epoch, as in ArcGIS.
