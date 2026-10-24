"""The imageserver app: COGs listed in STAC, served as ArcGIS ImageServers (docs/imageserver.md).

STAC_COLLECTION_URL=https://example.com/stac/items.json uvicorn --factory imageserver.main:create_app
"""

from __future__ import annotations

import logging
import os

from fastapi import FastAPI, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

from .catalog import Catalog
from .esri import EsriError
from .factory import ImageServerFactory

log = logging.getLogger("imageserver")
PREFIX = "/ugs/rest/services"


def create_app() -> FastAPI:
    urls = [u.strip() for u in os.environ.get("STAC_COLLECTION_URL", "").split(",") if u.strip()]
    if not urls:
        raise SystemExit("set STAC_COLLECTION_URL to one or more items.json, comma-separated")
    ttl = int(os.environ.get("CACHE_TTL", "300"))
    catalog = Catalog(urls, os.environ.get("ASSET_KEYS", "visual,cog,data").split(","), ttl)
    factory = ImageServerFactory(
        catalog=catalog,
        router_prefix=PREFIX,
        cache_ttl=ttl,
        max_image=int(os.environ.get("MAX_IMAGE_SIZE", "4100")),
    )
    public_url = os.environ.get("PUBLIC_URL", "").rstrip("/")

    app = FastAPI(title="imageserver", openapi_url=None, docs_url=None, redoc_url=None)
    app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["GET", "HEAD", "POST"])
    app.include_router(factory.router, prefix=PREFIX)

    @app.exception_handler(EsriError)
    def esri_error(request: Request, e: EsriError):
        return JSONResponse(e.body(), headers={"Cache-Control": "no-store"})

    @app.exception_handler(Exception)
    def internal(request: Request, e: Exception):
        log.error("request failed: %s", request.url, exc_info=e)
        return JSONResponse(EsriError(500, "Internal error").body(), headers={"Cache-Control": "no-store"})

    @app.get("/healthz")
    def healthz():
        return {"ok": True}

    @app.get("/ugs/rest/info")
    def info():
        return {"currentVersion": 10.91, "fullVersion": "10.9.1", "authInfo": {"isTokenBasedSecurity": False}}

    @app.get(PREFIX)
    def services():
        return {
            "currentVersion": 10.91,
            "folders": [],
            "services": [{"name": id, "type": "ImageServer"} for id in catalog.entries()],
        }

    @app.get("/")
    def root(request: Request):
        """Each service and the URL to add to Map Viewer."""
        base = public_url or str(request.base_url).rstrip("/")
        return {
            "services": [
                {
                    "id": id,
                    "title": e.item.get("properties", {}).get("title") or id,
                    "url": f"{base}{PREFIX}/{id}/ImageServer",
                }
                for id, e in catalog.entries().items()
            ]
        }

    return app
