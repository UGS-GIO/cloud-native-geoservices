"""The ImageServer routes, as a titiler factory: each Esri operation maps to one rio-tiler call on
the item's COG asset (exportImage to part, identify to point, tile to tile, metadata to info)."""

from __future__ import annotations

import hashlib
import json
import threading
from collections.abc import Callable
from urllib.parse import urlencode

import numpy
import rasterio
from attrs import define, field
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
    max_image: int = 4100
    cache_ttl: int = 300
    _layers: dict = field(factory=dict, init=False)
    _lock: threading.Lock = field(factory=threading.Lock, init=False)

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

    def open(self, entry: Entry) -> STACReader:
        return self.reader(entry.url, item=entry.item, include_assets={entry.asset})

    def layer(self, entry: Entry) -> Layer:
        """The COG's description, read once per item version."""
        key = (entry.item["id"], entry.version)
        with self._lock:
            if key in self._layers:
                return self._layers[key]
        with Reader(entry.item["assets"][entry.asset]["href"]) as cog:
            info = cog.info()
            block = cog.dataset.block_shapes[0][::-1]
            compression = cog.dataset.tags(ns="IMAGE_STRUCTURE").get("COMPRESSION", "")
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
        )
        with self._lock:
            self._layers[key] = layer
        return layer

    @staticmethod
    def drawable(layer: Layer):
        # How to show other pixel types (a stretch, a color map) is not decided yet.
        if layer.dtype != "uint8":
            raise EsriError(400, "Unable to export image", [f"Pixel type {layer.dtype} is not supported"])

    def respond(self, request: Request, entry: Entry, body: bytes | dict, media_type: str = "") -> Response:
        """A cacheable answer: an ETag of the item version and the request, and Cache-Control."""
        key = f"{entry.version}\0{request.url.path}\0{request.url.query}"
        tag = f'"{hashlib.sha256(key.encode()).hexdigest()[:32]}"'
        headers = {
            "ETag": tag,
            "Cache-Control": f"public, max-age={self.cache_ttl}, stale-while-revalidate={self.cache_ttl}",
        }
        if request.headers.get("if-none-match") == tag:
            return Response(status_code=304, headers=headers)
        if isinstance(body, dict):
            indent = 2 if request.query_params.get("f") == "pjson" else None
            return Response(json.dumps(body, indent=indent), media_type="application/json", headers=headers)
        return Response(body, media_type=media_type, headers=headers)

    def metadata(self):
        @self.router.get("/{id}/ImageServer", name="service")
        def service(request: Request, entry: Entry = Depends(self.path_dependency)):
            return self.respond(request, entry, esri.service(self.layer(entry), self.max_image))

        @self.router.get("/{id}/ImageServer/keyProperties", name="keyProperties")
        def key_properties(request: Request, entry: Entry = Depends(self.path_dependency)):
            return self.respond(request, entry, esri.key_properties(self.layer(entry)))

        @self.router.get("/{id}/ImageServer/rasterFunctionInfos", name="rasterFunctionInfos")
        def raster_functions(request: Request, entry: Entry = Depends(self.path_dependency)):
            return self.respond(request, entry, esri.RASTER_FUNCTIONS)

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
            if p.get("adjustaspectratio", "true").lower() != "false":
                box = esri.adjust_aspect(box, w, h)
            fmt = p.get("format", "jpgpng").lower()
            fmt = fmt if fmt in esri.FORMATS else "jpgpng"
            if p.get("f", "image").lower() in ("json", "pjson"):
                href = str(request.url.replace(query=urlencode({**p, "f": "image", "format": fmt})))
                return self.respond(
                    request,
                    entry,
                    {"href": href, "width": w, "height": h, "extent": esri.extent(box, image_sr), "scale": 0},
                )
            self.drawable(layer)
            resampling = esri.INTERPOLATION.get(p.get("interpolation", ""), "bilinear")
            with rasterio.Env(**env), self.open(entry) as src:
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
            output = {"jpg": ImageType.jpeg, "jpgpng": None}.get(fmt, ImageType.png)
            quality = int(p.get("compressionquality") or 75)
            options = {"quality": quality} if output != ImageType.png else {}
            content, media_type = self.render_func(
                img, output_format=output, add_mask=fmt != "jpg", **options
            )
            return self.respond(request, entry, content, media_type)

    def identify(self):
        @self.router.api_route("/{id}/ImageServer/identify", methods=["GET", "POST"], name="identify")
        def identify(
            request: Request,
            entry: Entry = Depends(self.path_dependency),
            p: dict = Depends(params),
            env: dict = Depends(self.environment_dependency),
        ):
            self.layer(entry)
            x, y, crs = esri.parse_point(p.get("geometry"), WEB_MERCATOR)
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
                request,
                entry,
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
            level: int,
            row: int,
            col: int,
            entry: Entry = Depends(self.path_dependency),
            env: dict = Depends(self.environment_dependency),
        ):
            layer = self.layer(entry)
            self.drawable(layer)
            if level > layer.maxzoom:
                return not_found(request)
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
            return self.respond(request, entry, content, media_type)
