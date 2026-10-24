# Build from the repository root: docker build -f imageserver/Dockerfile -t imageserver .
# rasterio's wheel carries GDAL; it needs only libexpat from the system.
FROM python:3.12-slim-bookworm@sha256:6a0e06f5ba0b373d051aabd3fffb9b873f27f9a2a3a14bcbe37dee6f0e3bbc70 AS build
COPY --from=ghcr.io/astral-sh/uv:0.12.12@sha256:73d2665b478d8fa2de1cf105c6841f8e9cb6b09e568fc7700440c09f8fcd7ac4 /uv /bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
WORKDIR /app
COPY imageserver/pyproject.toml imageserver/uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY imageserver/src ./src
RUN uv sync --frozen --no-dev --no-editable

FROM python:3.12-slim-bookworm@sha256:6a0e06f5ba0b373d051aabd3fffb9b873f27f9a2a3a14bcbe37dee6f0e3bbc70
RUN apt-get update && apt-get install -y --no-install-recommends libexpat1 && rm -rf /var/lib/apt/lists/*
COPY --from=build /app/.venv /app/.venv
# The commit, for the ETags: docker build --build-arg GIT_SHA=$(git rev-parse HEAD)
ARG GIT_SHA=""
# glibc's default of one malloc arena per thread kept freed image buffers until 512 MB ran out.
ENV PATH=/app/.venv/bin:$PATH PORT=8080 PYTHONUNBUFFERED=1 GIT_SHA=$GIT_SHA MALLOC_ARENA_MAX=2
USER nobody
EXPOSE 8080
CMD ["sh", "-c", "exec uvicorn --factory imageserver.main:create_app --host 0.0.0.0 --port $PORT --no-access-log"]
