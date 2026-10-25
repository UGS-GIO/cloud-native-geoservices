"""The ImageServer routes, as a titiler factory: each Esri operation maps to one rio-tiler call on
the item's COG asset (exportImage to part, identify to point, tile to tile, metadata to info)."""

from __future__ import annotations

import hashlib
import json
import math
import os
import pathlib
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

DEPENDENCIES = ("titiler.core", "rio-tiler", "rasterio")


def _version(dist: str) -> str:
    try:
        return version(dist)
    except PackageNotFoundError:
        return ""


def _build(
    root: pathlib.Path = pathlib.Path(__file__).parent, version: Callable[[str], str] = _version
) -> str:
    """The commit, or a hash of this package's files and its readers' versions: a new build can
    answer the same request differently."""
    if sha := os.environ.get("GIT_SHA"):
        return sha
    h = hashlib.sha256()
    for f in sorted(root.rglob("*")):
        if f.is_file() and "__pycache__" not in f.parts:
            h.update(f"{f.relative_to(root)}\0".encode() + f.read_bytes())
    h.update(json.dumps([version(d) for d in DEPENDENCIES]).encode())
    return h.hexdigest()[:16]


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


def blank(bands: int, h: int, w: int, dtype="uint8") -> numpy.ma.MaskedArray:
    """Fully masked zeros: masked_all leaves whatever memory held, and the PNG changes with it."""
    return numpy.ma.array(numpy.zeros((bands, h, w), dtype), mask=True)


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
    cdn_ttl: int = 300
    max_exports: int = 1
    export_wait: float = 10
    _layers: dict = field(factory=dict, init=False)
    _lock: threading.Lock = field(factory=threading.Lock, init=False)
    # Big exports take most of the memory: this many render at once, the rest wait export_wait.
    _exports: threading.Semaphore = field(
        init=False, default=Factory(lambda self: threading.Semaphore(self.max_exports), takes_self=True)
    )

    def path_dependency(self, id: str = Path(description="STAC item id")) -> Entry:
        entry = self.catalog.get(id)
        if entry is None:
            raise EsriError(404, f"Service {id}/ImageServer not found")
        return entry

    def register_routes(self):
        self.export_image()
        self.identify()
        self.tile()
        self.metadata()  # last: its catch-all route answers any other operation

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
        if cog is None:  # the HEAD failed: keep the version last seen
            cog = hit[1] if hit else ""
        if hit and hit[:2] == (entry.version, cog):
            with self._lock:
                self._layers[id] = (*hit[:2], now, hit[3])
            return hit[3]
        with Reader(entry.item["assets"][entry.asset]["href"]) as src:
            info = src.info()
            block = src.dataset.block_shapes[0][::-1]
            overviews = src.dataset.overviews(1)
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
            overviews=overviews,
            stats=stats if len(stats) == len(bands) else [],
            copyright=props.get("ugs:author", ""),
            version=hashlib.sha256(f"{entry.version}\0{cog}".encode()).hexdigest()[:16],
        )
        with self._lock:
            self._layers[id] = (entry.version, cog, now, layer)
        return layer

    def read(self, entry: Entry, layer: Layer, env: dict, box, crs, w: int, h: int, resampling: str):
        """The w x h image of box, or None where it misses the COG. Only the part of box over the COG
        is read: rio-tiler warps the whole box at the COG's resolution, terabytes for a world extent."""
        ext = layer.extent if crs == WEB_MERCATOR else transform_bounds(WEB_MERCATOR, crs, *layer.extent)
        cut = (max(box[0], ext[0]), max(box[1], ext[1]), min(box[2], ext[2]), min(box[3], ext[3]))
        if cut[0] >= cut[2] or cut[1] >= cut[3]:
            return None
        xres, yres = (box[2] - box[0]) / w, (box[3] - box[1]) / h
        c0, r0 = round((cut[0] - box[0]) / xres), round((box[3] - cut[3]) / yres)
        c1, r1 = max(c0 + 1, round((cut[2] - box[0]) / xres)), max(r0 + 1, round((box[3] - cut[1]) / yres))
        c0, r0, c1, r1 = min(c0, w - 1), min(r0, h - 1), min(c1, w), min(r1, h)
        # Source pixels GDAL reads: the cut at the coarsest overview no coarser than the output.
        src = layer.width * layer.height * (cut[2] - cut[0]) * (cut[3] - cut[1])
        src /= (ext[2] - ext[0]) * (ext[3] - ext[1])
        out = (c1 - c0) * (r1 - r0)
        f = max(o for o in (1, *layer.overviews) if o * o <= max(1, src / out))
        if src / f**2 > max(16 * out, self.max_image**2):
            raise EsriError(400, "Unable to export image", ["The raster has no overviews for this scale"])
        with rasterio.Env(**env), self.open(entry) as reader:
            try:
                img = reader.part(
                    cut,
                    dst_crs=crs,
                    bounds_crs=crs,
                    width=c1 - c0,
                    height=r1 - r0,
                    assets=[entry.asset],
                    resampling_method=resampling,
                    reproject_method=resampling,
                    **WARP,
                )
            except TileOutsideBounds:
                return None
        if (c0, r0, c1, r1) == (0, 0, w, h):
            return img
        data = blank(img.count, h, w, img.array.dtype)
        data[:, r0:r1, c0:c1] = img.array
        alpha = None
        if img.alpha_mask is not None:
            alpha = numpy.zeros((h, w), img.alpha_mask.dtype)
            alpha[r0:r1, c0:c1] = img.alpha_mask
        return ImageData(data, alpha_mask=alpha, bounds=box, crs=crs)

    @staticmethod
    def drawable(layer: Layer):
        # How to show other pixel types (a stretch, a color map) is not decided yet.
        if layer.dtype != "uint8":
            raise EsriError(400, "Unable to export image", [f"Pixel type {layer.dtype} is not supported"])

    def answer(self, request: Request, layer: Layer, p: dict, body: Callable[[], dict | tuple]) -> Response:
        """Tagged with the build, the item, the COG and the request; body() runs only without a 304."""
        key = json.dumps([BUILD, layer.version, request.url.path, sorted(p.items())])
        tag = f'"{hashlib.sha256(key.encode()).hexdigest()[:32]}"'
        headers = {
            "ETag": tag,
            "Cache-Control": f"public, max-age={self.cache_ttl}, s-maxage={self.cdn_ttl}, "
            f"stale-while-revalidate={self.cache_ttl}",
        }
        inm = request.headers.get("if-none-match")
        if inm and request.method in ("GET", "HEAD") and matches(inm, tag):
            return Response(status_code=304, headers=headers)
        out = body()
        if isinstance(out, Response):
            return out
        if isinstance(out, dict):
            out = json.dumps(out, indent=2 if p.get("f") == "pjson" else None), "application/json"
        return Response(out[0], media_type=out[1], headers=headers)

    def metadata(self):
        """Answers from the layer alone. ArcGIS Server answers an unknown operation with the service."""
        service = lambda layer: esri.service(layer, self.max_image)
        either = ["GET", "POST"]
        for op, body, methods in (
            ("", service, ["GET"]),
            ("keyProperties", esri.key_properties, ["GET"]),
            ("rasterFunctionInfos", lambda layer: esri.RASTER_FUNCTIONS, ["GET"]),
            ("legend", esri.legend, either),
            ("statistics", lambda layer: {"statistics": []}, either),
            ("histograms", lambda layer: {"histograms": []}, either),
            ("rasterAttributeTable", lambda layer: {}, either),
            ("{op:path}", service, either),
        ):
            name = {"": "service", "{op:path}": "unknown"}.get(op, op)
            path = f"/{{id}}/ImageServer/{op}".rstrip("/")
            self.router.add_api_route(path, self.static(body), methods=methods, name=name)

    def static(self, body: Callable[[Layer], dict]) -> Callable:
        def route(request: Request, entry: Entry = Depends(self.path_dependency), p: dict = Depends(params)):
            layer = self.layer(entry)
            return self.answer(request, layer, p, lambda: body(layer))

        return route

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
                return self.answer(request, layer, p, lambda: body)
            self.drawable(layer)
            resampling = esri.INTERPOLATION.get(p.get("interpolation", ""), "bilinear")
            output = {"jpg": ImageType.jpeg, "jpgpng": None}.get(fmt, ImageType.png)
            quality = esri.parse_quality(p.get("compressionquality"))
            options = {"quality": quality} if output != ImageType.png else {}

            def draw():
                if not self._exports.acquire(timeout=self.export_wait):
                    raise EsriError(503, "Unable to export image", ["The server is busy, try again"])
                try:
                    img = self.read(entry, layer, env, box, image_sr, w, h, resampling)
                    if img is None:
                        img = ImageData(blank(len(layer.band_names), h, w))
                    return self.render_func(img, output_format=output, add_mask=fmt != "jpg", **options)
                finally:
                    self._exports.release()

            return self.answer(request, layer, p, draw)

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

            def pixel():
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
                (mx,), (my,) = transform(crs, WEB_MERCATOR, [x], [y]) if crs != WEB_MERCATOR else ((x,), (y,))
                return {
                    "objectId": 0,
                    "name": "Pixel",
                    "value": value,
                    "location": {"x": mx, "y": my, "spatialReference": esri.spatial_reference(WEB_MERCATOR)},
                    "properties": None,
                    "catalogItems": None,
                    "catalogItemVisibilities": [],
                }

            return self.answer(request, layer, p, pixel)

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
            if not (level <= layer.maxzoom and WEB_MERCATOR_QUAD.is_valid(col, row, level)):
                return not_found(request)

            def draw():
                box = WEB_MERCATOR_QUAD.xy_bounds(col, row, level)
                img = self.read(entry, layer, env, box, WEB_MERCATOR, 256, 256, "bilinear")
                if img is None:
                    return not_found(request)
                return self.render_func(img, output_format=None, quality=75)

            return self.answer(request, layer, {}, draw)
