# Getting started

## Install

The server needs Go 1.26.3 or later to build (the version in `go.mod`). Every merge to `main` makes a tagged release, so
`@latest` is the newest release.

```bash
go install github.com/UGS-GIO/cloud-native-geoservices/cmd/featureserver@latest
```

You can also build a Docker image from the repository. The image is a static binary on
distroless, with no shell.

```bash
git clone https://github.com/UGS-GIO/cloud-native-geoservices
cd cloud-native-geoservices
docker build -t featureserver .
```

## Serve a folder

Put your GeoParquet files in one folder. Each `*.parquet` file is a layer, and its file name
without the extension is the layer id. The files must meet the
[data requirements](layer-sources.md#data-requirements).

```bash
LAYERS_DIR=./data featureserver
```

With Docker, mount the folder read-only:

```bash
docker run -p 8080:8080 -v "$PWD/data:/data:ro" -e LAYERS_DIR=/data featureserver
```

Open `http://localhost:8080/`. It lists each layer with its URL. A layer URL has this form:

```text
http://localhost:8080/ugs/rest/services/{id}/{render}/FeatureServer/0
```

A layer with no renders has the render `default`. To add symbols, see
[renders](layer-sources.md#renders).

Check a layer with a count query:

```bash
curl 'http://localhost:8080/ugs/rest/services/faults/default/FeatureServer/0/query?where=1=1&returnCountOnly=true&f=json'
```

## Add a layer to ArcGIS

### Map Viewer

Map Viewer does not accept an `http` URL in "Add layer from URL". It accepts one in the page URL:

```text
https://www.arcgis.com/apps/mapviewer/index.html?url=http://localhost:8080/ugs/rest/services/{id}/default/FeatureServer/0
```

When the server has an `https` URL, use "Add layer from URL" as for any feature layer.

### ArcGIS Pro

In ArcGIS Pro, select **Add Data** > **Data From Path** and paste the layer URL.

### Other clients

Experience Builder, StoryMaps and other clients of the ArcGIS Maps SDK for JavaScript use the same
layer URL. See [Querying](querying.md) for what the query endpoint supports.

### ImageServer

Add an ImageServer the same way, with a URL of the form
`https://api.geology.utah.gov/ugs/rest/services/{id}/ImageServer`. See [ImageServer](imageserver.md).

## Next steps

- To serve files from a CDN or a bucket, use a [config file or STAC](layer-sources.md).
- To run it in production, read [Deployment](deployment.md) and [Performance](performance.md).
