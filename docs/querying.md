# Querying

The server implements the read-only part of the ArcGIS REST FeatureServer API that ArcGIS clients
use to draw, query and identify features. Where the server and an ArcGIS Online hosted layer can
answer the same request, the server gives ArcGIS Online's answer. The
[conformance suite](conformance.md) checks this against recorded ArcGIS Online answers.

## Endpoints

| Path | Returns |
|---|---|
| `/` | Every layer and render, with its URL |
| `/healthz` | `{"ok": true}` |
| `/ugs/rest/info` | Server info. `authInfo.isTokenBasedSecurity` is `false`, as on an ArcGIS Server without security. |
| `/ugs/rest/services/{id}/{render}/FeatureServer` | Service description |
| `/ugs/rest/services/{id}/{render}/FeatureServer/layers` | All layers (there is one) |
| `/ugs/rest/services/{id}/{render}/FeatureServer/0` | Layer description: fields, extent, renderer |
| `/ugs/rest/services/{id}/{render}/FeatureServer/0/query` | Query |

Each service has one layer, with id 0. The path prefix `/ugs/rest/services` is fixed. There is no
token service.

The description endpoints accept `f=json` (the default), `f=pjson` (indented) and `callback`
(JSONP). Another `f` gives error 400. The layer description reports `currentVersion` 12,
`maxRecordCount` (`MAX_RECORDS`), `tileMaxRecordCount` (`TILE_MAX_RECORDS`),
`supportedQueryFormats: "JSON, geoJSON, PBF"` and `supportsCoordinatesQuantization: true`.

## Query parameters

`/query` accepts GET and POST. A POST body can be form-encoded or JSON. When a parameter is in
both the query string and the body, the query string wins.

The server reads values as ArcGIS Online reads them. Booleans and `f` can be in any case, so
`returnIdsOnly=True` works. A boolean other than true or false gives error 400.

A query needs a `where` unless it has `objectIds`, `geometry` or `outStatistics`. Without one, it
gives error 400.

### Filters

| Parameter | Notes |
|---|---|
| `where` | SQL subset. See [where clauses](#where-clauses). |
| `objectIds` | Comma list of `feature_id` values. At most `MAX_OBJECT_IDS`. |
| `geometry` | Envelope, point, multipoint, polyline or polygon, as Esri JSON. Also `xmin,ymin,xmax,ymax` or `x,y`. |
| `geometryType` | Accepted. The server reads the type from the geometry itself. |
| `inSR` | Spatial reference of `geometry`. A `spatialReference` in the geometry wins. See [spatial references](#spatial-references). |
| `spatialRel` | `esriSpatialRelIntersects` (default), `esriSpatialRelEnvelopeIntersects`, `esriSpatialRelContains`, `esriSpatialRelWithin`. Other values give error 400. |
| `distance`, `units` | Buffer the query geometry. See [distance](#distance). |

As in ArcGIS Server, `esriSpatialRelContains` means "the query geometry contains the feature",
and `esriSpatialRelWithin` means "the query geometry is within the feature".

### Output

| Parameter | Notes |
|---|---|
| `outFields` | Comma list, or `*` (default). Names match in any case. An unknown field gives error 400. |
| `returnGeometry` | `true` (default) or `false` |
| `outSR` | See [spatial references](#spatial-references) |
| `geometryPrecision` | Decimal places. 0 or less rounds to whole numbers. More than 20 keeps full precision. |
| `maxAllowableOffset` | Simplification tolerance, in the units of `outSR`. Picks the simplification level and the [overview](layer-sources.md#overviews). Ignored for point layers. |
| `orderByFields` | For example `name ASC, age DESC`. See [ordering](#ordering). |
| `resultOffset`, `resultRecordCount` | Paging. Whole numbers, 0 or more, else error 400. See [paging](#paging). |
| `resultType` | `tile` uses the tile page size, `TILE_MAX_RECORDS` |
| `returnCountOnly`, `returnIdsOnly`, `returnExtentOnly` | Count, ids or extent instead of features |
| `returnDistinctValues` | Distinct rows of `outFields` |
| `outStatistics`, `groupByFieldsForStatistics` | See [statistics](#statistics) |
| `f` | `json` (default), `pjson`, `geojson` or `pbf`. Another value gives error 400. |
| `quantizationParameters` | For `f=json` and `f=pbf`. See [quantization](#quantization). |
| `callback` | JSONP. With `callback`, `f=pbf` writes JSON. |

## Output formats

| `f` | Content type | Notes |
|---|---|---|
| `json` | `application/json` | Esri JSON |
| `pjson` | `application/json` | Esri JSON, indented |
| `geojson` | `application/json` | A GeoJSON FeatureCollection |
| `pbf` | `application/x-protobuf` | Esri FeatureCollection protocol buffer, as Map Viewer requests it |

A query response writes `exceededTransferLimit` only when it is `true`, and leaves out `hasZ` and
`hasM`.

With `f=geojson`, every feature has `type`, `geometry` (`null` when `returnGeometry=false`) and
`properties`, and `id` when the id field is in the output. A count, ids or an extent is a
FeatureCollection with no features: the count and ids are in `properties`, and the extent is in
`bbox`. The collection has a `crs` member when the request sets `outSR`, and for an extent.

With `f=pbf`, a `returnExtentOnly` answer is one polygon feature, the extent's ring, with the
count as an attribute when the request also sets `returnCountOnly`.

## Paging

A page of features has at most `MAX_RECORDS` features (default 2000), with or without geometry.
This is the layer's `maxRecordCount`. `resultRecordCount` can ask for fewer, not more. A request
with `resultType=tile` gets pages of up to `TILE_MAX_RECORDS` (default 8000), the layer's
`tileMaxRecordCount`, so a dense map tile answers in one request.

When more features match than the page holds, the response sets `exceededTransferLimit` to
`true`. The client then asks for the next page with `resultOffset`. A page of large geometries
also ends early when its encoded size passes `MAX_RESPONSE_MB` (default 30).

Counts, ids, extents and statistics are not capped at `MAX_RECORDS`. A count and an extent cover
every match and ignore `resultOffset` and `resultRecordCount`. Ids and statistics are paged only
when the request sets `resultRecordCount`.

How the server reads a page depends on the request:

- With no `where` (or only `1=1`) and no order other than `feature_id`, the server pages by row
  number and reads only the rows of the page.
- With a `where` or another order, the server reads up to `MAX_GEOMETRY_ROWS` rows with geometry
  (default 50000), then filters, orders and pages them. If it stops at that cap, the response sets
  `exceededTransferLimit`. Without geometry (`returnGeometry=false`, no spatial filter), this cap
  does not apply.
- Counts, ids and extents under a spatial filter cover every row. The server reads them in chunks
  of `MAX_GEOMETRY_ROWS` and keeps no geometry.
- A count or extent with no filter comes from the file footer, with no data read. An extent with
  a filter comes from the bbox covering columns when the file has them.

When a paged request has `maxAllowableOffset`, the server reads past the page. Of the features
smaller than the tolerance, it keeps one per tolerance-sized cell and symbol class. With
`resultType=tile`, if the tile is still over the page size, the server keeps the largest features
and does not set `exceededTransferLimit`, so Map Viewer does not split the tile again.

## Spatial references

The server stores and reads EPSG:4326. It supports these WKIDs:

| WKID | `inSR` | `outSR` |
|---|---|---|
| `4326` | Yes | Yes |
| `4269` | Yes | Yes. Converted with WGS_1984_(ITRF00)_To_NAD_1983, as on ArcGIS Online. |
| `102100`, `3857` | Yes | Yes. Labeled `102100` (latest `3857`). |
| `102113`, `3785` | Yes | Yes. Labeled `102113` (latest `3785`). |
| `900913` | Yes | No. Error 400, as on ArcGIS Online. |

Any other WKID gives error 400. Web Mercator extents clamp latitude to ±85.0511.

## Distance

With `distance`, the query geometry is buffered by that distance in `units`: `esriSRUnit_Meter`
(the default), `esriSRUnit_Kilometer`, `esriSRUnit_Foot`, `esriSRUnit_StatuteMile`,
`esriSRUnit_NauticalMile` or `esriSRUnit_USNauticalMile`. Another unit name gives error 400.

- With `esriSpatialRelIntersects`, a feature matches when it is within the distance of the
  geometry. Distances are measured on the WGS84 ellipsoid near the geometry, within 0.1% for
  tens of kilometers.
- With the other `spatialRel` values, the test uses the buffered geometry.

A negative distance, or one over about 19,800 km, gives error 400. The distance also grows the
bbox prefilter.

## Ordering

`orderByFields` sorts as ArcGIS Online does:

- Nulls are lowest: first in ascending order, last in descending order.
- Numbers sort by value.
- Strings sort as SQL Server's case-insensitive word sort: case is ignored, and hyphens and
  apostrophes count only when the rest is equal.

An unknown field gives error 400. A statistics query can also sort by an output statistic name.

Without `orderByFields`, a full page holds the first matches by object id. ArcGIS Online picks
them in its own index order.

## Statistics

`outStatistics` is a JSON list of `{"statisticType", "onStatisticField", "outStatisticFieldName"}`.
The types are `count`, `sum`, `min`, `max`, `avg`, `stddev` and `var`. Use
`groupByFieldsForStatistics` to group. Without `outStatisticFieldName`, the name is
`{TYPE}_{field}`, for example `COUNT_name`. An unknown type or field, or a name with a space,
gives error 400.

Statistics follow SQL aggregates:

- Each statistic uses the non-null values only. `count` counts them.
- `stddev` and `var` are the sample statistics. With fewer than two values, they are null.
- Every statistic other than `count` is null when there are no values.
- Without `orderByFields`, the groups are sorted by their values.

The result fields have ArcGIS Online's types: `count` is an integer, `min` and `max` have the type
of their source field, and the other statistics are doubles.

Statistics cover every row that passes the filters. Without a spatial filter they read no
geometry. A statistics query with no spatial filter is a full-layer scan and counts against
`MAX_SCANS`.

## Quantization

`quantizationParameters` apply to `f=json` and `f=pbf`, as on ArcGIS Online. `f=geojson` ignores
them. They accept `mode`, `tolerance`, `extent` and `originPosition` (`upperLeft` or `lowerLeft`).

- `view` mode steps by `tolerance` from the corner of `extent`. The default tolerance is the
  extent's width and height over 10000. The default extent is the layer extent.
- `edit` mode steps over the XY domain of the output spatial reference.

With `f=json`, the response has a `transform`, and coordinates are integer steps, each one a delta
from the one before. A geometry that collapses to one point comes back empty, with its envelope
center as `centroid`. `f=pbf` uses the same grid.

## Where clauses

`where` supports this SQL subset:

- Comparisons: `=`, `<>`, `!=`, `<`, `<=`, `>`, `>=`
- `AND`, `OR`, `NOT` and parentheses
- `[NOT] IN (...)`, `[NOT] BETWEEN ... AND ...`, `IS [NOT] NULL`
- `[NOT] LIKE`, with `%` and `_`, and an optional `ESCAPE`
- Arithmetic: `+`, `-`, `*`, `/`, and `||` to join strings
- `UPPER()`, `LOWER()` (also `UCASE()`, `LCASE()`)
- String and number literals, `TRUE`, `FALSE`, `NULL`
- `DATE '2020-01-01'` and `TIMESTAMP '2020-01-01 00:00:00'`, in UTC

The rules follow ArcGIS Online hosted layers:

- Field names match in any case. An unknown field gives error 400.
- Logic is SQL's three-valued logic. A comparison with a null is unknown, so neither it nor its
  `NOT` matches.
- A literal converts to the type of the field that it meets, for example `FIPS = '49035'` on a
  number field or `UPDATED > '2020-01-01'` on a date field. Other type mismatches give error 400.
- Strings compare in [word sort](#ordering) order, ignoring case. `LIKE` also ignores case.
- `LIKE` on a number or date reads it as SQL Server writes it as text, for example `38.7973`,
  `4.00911e+006` or `2011-08-11 09:00:00.0000000`.
- Integers divide to an integer. Integer arithmetic past 32 bits, or a division by zero, gives
  error 400.
- `1=1` is evaluated like any other condition. A `where` of only `1=1` selects all rows.

A `where` longer than `MAX_WHERE_LENGTH` characters, or one that does not parse, gives error 400.

## Errors

Errors use the Esri error body with HTTP status 200, as ArcGIS Server does:

```json
{"error": {"code": 400, "message": "'spatialRel' parameter is invalid", "details": []}}
```

| Code | Cause |
|---|---|
| 400 | Invalid parameter, or a request over a limit |
| 404 | Unknown layer id or render |
| 500 | Internal error. The server logs it. |
| 504 | The request took longer than `REQUEST_TIMEOUT_S` |

When the server sheds load, it uses HTTP status 503 with `Retry-After`. See
[Admission and 503](performance.md#admission-and-503). An unknown path gives HTTP 404.

## Differences from ArcGIS Online

These are the main differences from an ArcGIS Online hosted layer:

- Point layers return multipoint geometry in `f=json` and `f=geojson`.
- `outSR` and `inSR` support only the WKIDs in [spatial references](#spatial-references).
- `f=geojson` does not fully conform to RFC 7946.
- `f=pbf` lists fields in alphabetical order and leaves out `latestWkid`.
- Fields follow the schema order, not the `outFields` order.
- GeoParquet has no string lengths, so string fields report length 128.
- `returnCountOnly` with `returnDistinctValues` counts rows, not distinct values.
- Each multipolygon member's rings are written hole first.
- There is no support for `having`, `returnZ`, `returnM`, attachments, related records or edits.

The [conformance suite](conformance.md) records every difference that it finds in
[`known_gaps.json`](https://github.com/UGS-GIO/geoparquet-featureserver/blob/main/testdata/conformance/known_gaps.json)
and
[`known_gaps_large.json`](https://github.com/UGS-GIO/geoparquet-featureserver/blob/main/testdata/conformance/known_gaps_large.json),
each with a reason.
