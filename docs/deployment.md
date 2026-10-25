# Deployment

The server is stateless. All of its state is in memory caches that fill again after a restart, so
you can run as many copies as you need behind a load balancer.

This page is about `featureserver`. To build and run the ImageServer, see
[ImageServer](imageserver.md#running).

## Docker

The `Dockerfile` builds a static binary and copies it onto
`gcr.io/distroless/static-debian12:nonroot`. The image has no shell and runs as a non-root user.

```bash
docker build -t featureserver .
```

Serve a local folder:

```bash
docker run -p 8080:8080 -v "$PWD/data:/data:ro" -e LAYERS_DIR=/data featureserver
```

Serve files over HTTPS with a STAC ItemCollection:

```bash
docker run -p 8080:8080 \
  -e STAC_COLLECTION_URL=https://example.com/stac/items.json \
  -e PUBLIC_URL=https://features.example.com \
  featureserver
```

Set a memory limit on the container (`--memory`). The server sizes its caches from it. See
[Memory sizing](performance.md#memory-sizing).

## Cloud Run

Cloud Run gives each instance a cgroup memory limit, so the cache defaults follow the memory that
you set. The server reads `PORT` from the environment, which Cloud Run sets.

Build and push the image to a registry that Cloud Run can read, then deploy it:

```bash
IMAGE=REGION-docker.pkg.dev/PROJECT/REPOSITORY/featureserver

docker build -t "$IMAGE" .
docker push "$IMAGE"

gcloud run deploy featureserver \
  --image "$IMAGE" \
  --region REGION \
  --memory 2Gi \
  --cpu 2 \
  --allow-unauthenticated \
  --set-env-vars STAC_COLLECTION_URL=https://example.com/stac/items.json,PUBLIC_URL=https://features.example.com
```

!!! note
    `--set-env-vars` splits on commas. For a value that contains commas, such as
    `COMPRESSION=zstd,br,gzip`, choose another delimiter:
    `--set-env-vars "^@^COMPRESSION=zstd,br,gzip@CACHE_TTL=600"`.

Notes for Cloud Run:

- The data files must be readable over HTTPS with range requests. The server sends no
  credentials, so the files must be public. A public bucket or a CDN in front of the bucket
  works.
- Cloud Run sends `SIGTERM` before it stops an instance. The server stops accepting new
  connections and gives requests in flight up to 10 seconds to finish.
- Cloud Run limits concurrent requests per instance. The server also limits its own work, and
  returns 503 when it is full. See [Admission and 503](performance.md#admission-and-503).
- Put a CDN in front of the service. See [Put a CDN in front](performance.md#put-a-cdn-in-front).

## Health check

`GET /healthz` returns `{"ok": true}` with HTTP 200. It does not read the layer source or any data,
and the `MAX_IN_FLIGHT` limit never refuses it. Use it for startup and liveness probes.

To check that the layer source is readable, request `/`. It returns HTTP 502 when the server
cannot read the source and has no earlier copy.

## Logs

The server logs to stderr in `key=value` text, at `LOG_LEVEL` (default `warn`). At start it logs
the listen address, the layer source and the memory limit that it found. Failed requests and
recovered panics log at `error`. `LOG_LEVEL=debug` logs each range request with its size and time.

For CPU and memory profiles, set `PPROF_ADDR=localhost:6060` and use `go tool pprof`. Do not
expose that address publicly.
