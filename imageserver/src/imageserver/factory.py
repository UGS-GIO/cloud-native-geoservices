"""The ImageServer routes, as a titiler factory: each Esri operation maps to one rio-tiler call on
the item's COG asset (exportImage to part, identify to point, tile to tile, metadata to info)."""

from __future__ import annotations

import hashlib
import json
import math
import os
import threading
import time
from collections.abc import Callable
from importlib.metadata import PackageNotFoundError, version
from urllib.parse import urlencode

import numpy
import rasterio
from attrs import Factory, define, field
from fastapi import Depends, Path, Request
from fastapi.responses import Response
from morecantile import tms
from rasterio.warp import transform, transform_bounds
from rio_tiler.errors import PointOutsideBounds, TileOutsideBounds
from rio_tiler.io import Reader, STACReader
from rio_tiler.models import ImageData
from titiler.core.factory import BaseFactory
from titiler.core.resources.enums import ImageType
from titiler.core.utils import render_image

from . import esri
from .catalog import Catalog, Entry
from .esri import WEB_MERCATOR, EsriError, Layer


def _build() -> str:
    """This release and commit: a new one can answer the same request differently."""
    try:
        v = version("imageserver")
    except PackageNotFoundError:
        v = ""
    return f"{v} {os.environ.get('GIT_SHA', '')}".strip()


BUILD = _build()

WEB_MERCATOR_QUAD = tms.get("WebMercatorQuad")
# Always warp, with bounded memory: a same-CRS read past the COG's edge is otherwise a boundless
# read, which ignores the overviews and reads the full resolution.
WARP = {"vrt_options": {"warp_mem_limit": 64}}
STATS = (("min", "MINIMUM"), ("max", "MAXIMUM"), ("mean", "MEAN"), ("std", "STDDEV"))
GDAL_COMPRESSION = {
    "DEFLATE": "Deflate",
    "LZW": "LZW",
    "JPEG": "JPEG",
    "WEBP": "WebP",
    "ZSTD": "ZSTD",
    "LERC": "LERC",
}


async def params(request: Request) -> dict[str, str]:
    """Query and form parameters, with lowercase names: ArcGIS reads them in any case."""
    p = {k.lower(): v for k, v in request.query_params.items()}
    if request.method == "POST":
        p.update({k.lower(): v for k, v in (await request.form()).items() if isinstance(v, str)})
    return p


def matches(if_none_match: str, tag: str) -> bool:
    """If-None-Match: "*" or a list of tags, compared weakly (RFC 9110)."""
    tags = [t.strip().removeprefix("W/") for t in if_none_match.split(",")]
    return "*" in tags or tag in tags


def not_found(request: Request) -> Response:
    """ArcGIS Server's answer to a tile it does not have."""
    return Response(
        f"<html><body><pre>Cannot {request.method} {request.url.path}</pre></body></html>",
        status_code=404,
        media_type="text/html; charset=utf-8",
        headers={"Cache-Control": "no-store"},
    )


@define(kw_only=True)
class ImageServerFactory(BaseFactory):
    """`/{id}/ImageServer` and its operations, for each catalog item."""

    catalog: Catalog
    reader: type[STACReader] = STACReader
    render_func: Callable[..., tuple[bytes, str]] = render_image
    environment_dependency: Callable[..., dict] = lambda: {}  # noqa: PIE807 (FastAPI needs a signature)
    max_image: int = 2048
    cache_ttl: int = 300
    max_exports: int = 1
    _layers: dict = field(factory=dict, init=False)
    _lock: threading.Lock = field(factory=threading.Lock, init=False)
    # Big exports take most of the memory: this many render at once, the rest wait.
    _exports: threading.Semaphore = field(
        init=False, default=Factory(lambda self: threading.Semaphore(self.max_exports), takes_self=True)
    )

    def path_dependency(self, id: str = Path(description="STAC item id")) -> Entry:
        entry = self.catalog.get(id)
        if entry is None:
            raise EsriError(404, f"Service {id}/ImageServer not found")
        return entry

    def register_routes(self):
        self.metadata()
        self.export_image()
        self.identify()
        self.tile()
        self.unknown()

    def open(self, entry: Entry) -> STACReader:
        return self.reader(entry.url, item=entry.item, include_assets={entry.asset})

    def layer(self, entry: Entry) -> Layer:
        """The COG's description, read again when the item or the COG changes, checked every TTL."""
        id, now = entry.item["id"], time.monotonic()
        with self._lock:
            hit = self._layers.get(id)
        if hit and hit[0] == entry.version and now - hit[2] < self.cache_ttl:
            return hit[3]
        cog = self.catalog.cog_version(entry)
        if hit and hit[:2] == (entry.version, cog):
            with self._lock:
                self._layers[id] = (*hit[:2], now, hit[3])
            return hit[3]
        with Reader(entry.item["assets"][entry.asset]["href"]) as src:
            info = src.info()
            block = src.dataset.block_shapes[0][::-1]
            compression = src.dataset.tags(ns="IMAGE_STRUCTURE").get("COMPRESSION", "")
        bands = [i for i, ci in enumerate(info.colorinterp or []) if ci != "alpha"] or list(range(info.count))
        names = [
            info.colorinterp[i].capitalize()
            if info.colorinterp and info.colorinterp[i] in ("red", "green", "blue")
            else f"Band_{n + 1}"
            for n, i in enumerate(bands)
        ]
        box = transform_bounds(info.crs, WEB_MERCATOR, *info.bounds, densify_pts=21)
        pixel = (box[2] - box[0]) / info.width
        meta = [dict(info.band_metadata[i][1]) for i in bands]
        stats = [
            {k: float(m[f"STATISTICS_{v}"]) for k, v in STATS} for m in meta if "STATISTICS_MINIMUM" in m
        ]
        props = entry.item.get("properties", {})
        layer = Layer(
            name=entry.item["id"],
            description=props.get("description") or props.get("title") or "",
            extent=box,
            pixel_size=pixel,
            band_names=names,
            dtype=str(info.dtype),
            block=block,
            compression=GDAL_COMPRESSION.get(compression, "None"),
            width=info.width,
            height=info.height,
            maxzoom=WEB_MERCATOR_QUAD.zoom_for_res(pixel, zoom_level_strategy="upper"),
            stats=stats if len(stats) == len(bands) else [],
            copyright=props.get("ugs:author", ""),
            version=hashlib.sha256(f"{entry.version}\0{cog}".encode()).hexdigest()[:16],
        )
        with self._lock:
            self._layers[id] = (entry.version, cog, now, layer)
        return layer

    @staticmethod
    def drawable(layer: Layer):
        # How to show other pixel types (a stretch, a color map) is not decided yet.
        if layer.dtype != "uint8":
            raise EsriError(400, "Unable to export image", [f"Pixel type {layer.dtype} is not supported"])

    def headers(self, tag: str) -> dict[str, str]:
        return {
            "ETag": tag,
            "Cache-Control": f"public, max-age={self.cache_ttl}, stale-while-revalidate={self.cache_ttl}",
        }

    def validate(self, request: Request, layer: Layer, p: dict) -> tuple[str, Response | None]:
        """The ETag, of the build, the item, the COG and the request, and a 304 if the client has it."""
        key = json.dumps([BUILD, layer.version, request.url.path, sorted(p.items())])
        tag = f'"{hashlib.sha256(key.encode()).hexdigest()[:32]}"'
        inm = request.headers.get("if-none-match")
        if inm and request.method in ("GET", "HEAD") and matches(inm, tag):
            return tag, Response(status_code=304, headers=self.headers(tag))
        return tag, None

    def respond(self, tag: str, p: dict, body: bytes | dict, media_type: str = "") -> Response:
        if isinstance(body, dict):
            indent = 2 if p.get("f") == "pjson" else None
            body, media_type = json.dumps(body, indent=indent), "application/json"
        return Response(body, media_type=media_type, headers=self.headers(tag))

    def describe(self, request: Request, entry: Entry, p: dict, body: Callable[[Layer], dict]) -> Response:
        layer = self.layer(entry)
        tag, cached = self.validate(request, layer, p)
        return cached or self.respond(tag, p, body(layer))

    def metadata(self):
        @self.router.get("/{id}/ImageServer", name="service")
        def service(
            request: Request, entry: Entry = Depends(self.path_dependency), p: dict = Depends(params)
        ):
            return self.describe(request, entry, p, lambda layer: esri.service(layer, self.max_image))

        @self.router.get("/{id}/ImageServer/keyProperties", name="keyProperties")
        def key_properties(
            request: Request, entry: Entry = Depends(self.path_dependency), p: dict = Depends(params)
        ):
            return self.describe(request, entry, p, esri.key_properties)

        @self.router.get("/{id}/ImageServer/rasterFunctionInfos", name="rasterFunctionInfos")
        def raster_functions(
            request: Request, entry: Entry = Depends(self.path_dependency), p: dict = Depends(params)
        ):
            return self.describe(request, entry, p, lambda layer: esri.RASTER_FUNCTIONS)

    def export_image(self):
        @self.router.api_route("/{id}/ImageServer/exportImage", methods=["GET", "POST"], name="exportImage")
        def export_image(
            request: Request,
            entry: Entry = Depends(self.path_dependency),
            p: dict = Depends(params),
            env: dict = Depends(self.environment_dependency),
        ):
            layer = self.layer(entry)
            box = esri.parse_bbox(p.get("bbox"))
            bbox_sr = esri.parse_sr(p.get("bboxsr"), WEB_MERCATOR)
            image_sr = esri.parse_sr(p.get("imagesr"), WEB_MERCATOR)
            w, h = esri.parse_size(p.get("size"), self.max_image)
            if bbox_sr != image_sr:
                box = transform_bounds(bbox_sr, image_sr, *box, densify_pts=21)
                if not all(map(math.isfinite, box)):
                    raise EsriError(400, "Unable to export image", ["Invalid bbox"])
            if p.get("adjustaspectratio", "true").lower() != "false":
                box = esri.adjust_aspect(box, w, h)
            fmt = p.get("format", "jpgpng").lower()
            fmt = fmt if fmt in esri.FORMATS else "jpgpng"
            if p.get("f", "image").lower() in ("json", "pjson"):
                href = str(request.url.replace(query=urlencode({**p, "f": "image", "format": fmt})))
                body = {
                    "href": href,
                    "width": w,
                    "height": h,
                    "extent": esri.extent(box, image_sr),
                    "scale": 0,
                }
                return self.describe(request, entry, p, lambda layer: body)
            self.drawable(layer)
            tag, cached = self.validate(request, layer, p)
            if cached:
                return cached
            resampling = esri.INTERPOLATION.get(p.get("interpolation", ""), "bilinear")
            output = {"jpg": ImageType.jpeg, "jpgpng": None}.get(fmt, ImageType.png)
            options = (
                {"quality": esri.parse_quality(p.get("compressionquality"))}
                if output != ImageType.png
                else {}
            )
            with self._exports, rasterio.Env(**env), self.open(entry) as src:
                try:
                    img = src.part(
                        box,
                        dst_crs=image_sr,
                        bounds_crs=image_sr,
                        width=w,
                        height=h,
                        assets=[entry.asset],
                        resampling_method=resampling,
                        reproject_method=resampling,
                        **WARP,
                    )
                except TileOutsideBounds:
                    img = ImageData(numpy.ma.masked_all((len(layer.band_names), h, w), dtype="uint8"))
                except OverflowError:  # a bbox too big for GDAL's integer pixel window
                    raise EsriError(400, "Unable to export image", ["Invalid bbox"])
                content, media_type = self.render_func(
                    img, output_format=output, add_mask=fmt != "jpg", **options
                )
            return self.respond(tag, p, content, media_type)

    def identify(self):
        @self.router.api_route("/{id}/ImageServer/identify", methods=["GET", "POST"], name="identify")
        def identify(
            request: Request,
            entry: Entry = Depends(self.path_dependency),
            p: dict = Depends(params),
            env: dict = Depends(self.environment_dependency),
        ):
            layer = self.layer(entry)
            x, y, crs = esri.parse_point(p.get("geometry"), WEB_MERCATOR)
            tag, cached = self.validate(request, layer, p)
            if cached:
                return cached
            value = "NoData"
            with rasterio.Env(**env), self.open(entry) as src:
                try:
                    pt = src.point(x, y, coord_crs=crs, assets=[entry.asset])
                    if bool(numpy.all(pt.mask)):
                        value = ", ".join(
                            format(v, "g") if isinstance(v, float) else str(v) for v in pt.data.tolist()
                        )
                except PointOutsideBounds:
                    pass
            if crs != WEB_MERCATOR:
                (x,), (y,) = transform(crs, WEB_MERCATOR, [x], [y])
            return self.respond(
                tag,
                p,
                {
                    "objectId": 0,
                    "name": "Pixel",
                    "value": value,
                    "location": {"x": x, "y": y, "spatialReference": esri.spatial_reference(WEB_MERCATOR)},
                    "properties": None,
                    "catalogItems": None,
                    "catalogItemVisibilities": [],
                },
            )

    def tile(self):
        @self.router.get("/{id}/ImageServer/tile/{level}/{row}/{col}", name="tile")
        def tile(
            request: Request,
            level: str,
            row: str,
            col: str,
            entry: Entry = Depends(self.path_dependency),
            env: dict = Depends(self.environment_dependency),
        ):
            layer = self.layer(entry)
            self.drawable(layer)
            try:
                level, row, col = int(level), int(row), int(col)
            except ValueError:
                return not_found(request)
            if not (0 <= level <= layer.maxzoom and 0 <= row < 1 << level and 0 <= col < 1 << level):
                return not_found(request)
            tag, cached = self.validate(request, layer, {})
            if cached:
                return cached
            with rasterio.Env(**env), self.open(entry) as src:
                try:
                    img = src.tile(
                        col,
                        row,
                        level,
                        tilesize=256,
                        assets=[entry.asset],
                        resampling_method="bilinear",
                        reproject_method="bilinear",
                        **WARP,
                    )
                except TileOutsideBounds:
                    return not_found(request)
            content, media_type = self.render_func(img, output_format=None, quality=75)
            return self.respond(tag, {}, content, media_type)

    def unknown(self):
        """ArcGIS Server answers an operation it does not know with the service description."""

        @self.router.api_route("/{id}/ImageServer/{op:path}", methods=["GET", "POST"], name="unknown")
        def unknown(
            request: Request, entry: Entry = Depends(self.path_dependency), p: dict = Depends(params)
        ):
            return self.describe(request, entry, p, lambda layer: esri.service(layer, self.max_image))
