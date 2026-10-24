# Build from the repository root: docker build -f imageserver/Dockerfile -t imageserver .
# rasterio's wheel carries GDAL; it needs only libexpat from the system.
FROM python:3.12-slim-bookworm AS build
COPY --from=ghcr.io/astral-sh/uv:0.12.12 /uv /bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
WORKDIR /app
COPY imageserver/pyproject.toml imageserver/uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY imageserver/src ./src
RUN uv sync --frozen --no-dev --no-editable

FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends libexpat1 && rm -rf /var/lib/apt/lists/*
COPY --from=build /app/.venv /app/.venv
ENV PATH=/app/.venv/bin:$PATH PORT=8080 PYTHONUNBUFFERED=1
USER nobody
EXPOSE 8080
CMD ["sh", "-c", "exec uvicorn --factory imageserver.main:create_app --host 0.0.0.0 --port $PORT --no-access-log"]
