"""ArcGIS REST JSON for an ImageServer: the service description, spatial references, request
parameters and errors, following the recorded ArcGIS Server answers (docs/imageserver.md)."""

from __future__ import annotations

import copy
import json
import math
from dataclasses import dataclass, field
from importlib import resources

from rasterio.crs import CRS
from rasterio.errors import CRSError

DEFAULTS = json.loads(resources.files(__package__).joinpath("defaults/service.json").read_text())
WEB_MERCATOR = CRS.from_epsg(3857)
# Esri's own codes for Web Mercator.
MERCATOR_WKIDS = {3857, 102100, 102113, 900913, 3785}

# exportImage formats; any other is jpgpng, as on ArcGIS Server.
FORMATS = {"png", "png8", "png24", "png32", "jpg", "jpgpng"}
INTERPOLATION = {
    "RSP_NearestNeighbor": "nearest",
    "RSP_BilinearInterpolation": "bilinear",
    "RSP_CubicConvolution": "cubic",
    "RSP_Majority": "mode",
}


class EsriError(Exception):
    """An error ArcGIS reports in the body, with HTTP 200."""

    def __init__(self, code: int, message: str, details: list[str] | None = None):
        super().__init__(message)
        self.code, self.message, self.details = code, message, details or []

    def body(self) -> dict:
        return {"error": {"code": self.code, "message": self.message, "details": self.details}}


@dataclass
class Layer:
    """What the COG and its STAC item say about a service."""

    name: str
    description: str
    extent: tuple[float, float, float, float]  # Web Mercator
    pixel_size: float  # Web Mercator meters at full resolution
    band_names: list[str]
    dtype: str
    block: tuple[int, int]
    compression: str
    width: int
    height: int
    maxzoom: int
    copyright: str = ""
    version: str = ""  # changes with the item and the COG
    stats: list[dict] = field(default_factory=list)  # per band: min, max, mean, std


PIXEL_TYPES = {
    "uint8": "U8",
    "int8": "S8",
    "uint16": "U16",
    "int16": "S16",
    "uint32": "U32",
    "int32": "S32",
    "float32": "F32",
    "float64": "F64",
}


def spatial_reference(crs: CRS) -> dict:
    epsg = crs.to_epsg()
    if epsg == 3857:
        return {"wkid": 102100, "latestWkid": 3857}
    if epsg:
        return {"wkid": epsg, "latestWkid": epsg}
    return {"wkt": crs.to_wkt()}


def parse_sr(value: str | None, default: CRS) -> CRS:
    """A spatial reference parameter: a wkid, or JSON with wkid, latestWkid or wkt."""
    if value is None or value.strip() == "":
        return default
    v = value.strip()
    try:
        if v.startswith("{"):
            doc = json.loads(v)
            if "wkt" in doc:
                return CRS.from_wkt(doc["wkt"])
            v = str(doc.get("latestWkid") or doc["wkid"])
        wkid = int(v)
        return WEB_MERCATOR if wkid in MERCATOR_WKIDS else CRS.from_epsg(wkid)
    except (ValueError, KeyError, TypeError, CRSError):
        raise EsriError(400, "Invalid spatial reference", [f"Unknown spatial reference: {value}"])


def parse_bbox(value: str | None) -> tuple[float, float, float, float]:
    if not value:
        raise EsriError(400, "Unable to export image", ["bbox must be specified"])
    try:
        if value.strip().startswith("{"):
            d = json.loads(value)
            box = (d["xmin"], d["ymin"], d["xmax"], d["ymax"])
        else:
            box = value.split(",")
        xmin, ymin, xmax, ymax = map(float, box)
    except (ValueError, KeyError, TypeError):
        raise EsriError(400, "Unable to export image", ["Invalid bbox"])
    if not all(map(math.isfinite, (xmin, ymin, xmax, ymax))) or xmax <= xmin or ymax <= ymin:
        raise EsriError(400, "Unable to export image", ["Invalid bbox"])
    return xmin, ymin, xmax, ymax


def parse_size(value: str | None, limit: int) -> tuple[int, int]:
    try:
        w, h = (int(float(p)) for p in (value or "400,400").split(","))
    except (ValueError, OverflowError):
        w = h = 0
    if w <= 0 or h <= 0:
        raise EsriError(400, "Unable to export image", ["Invalid size"])
    if w > limit or h > limit:
        raise EsriError(
            400, "Unable to export image", [f"Requested image exceeds the size limit of {limit} x {limit}"]
        )
    return w, h


def adjust_aspect(box: tuple[float, float, float, float], w: int, h: int) -> tuple[float, ...]:
    """Grows the box about its center to the image's aspect ratio, as adjustAspectRatio does."""
    xmin, ymin, xmax, ymax = box
    bw, bh = xmax - xmin, ymax - ymin
    if bw / bh < w / h:
        grow = (bh * w / h - bw) / 2
        return xmin - grow, ymin, xmax + grow, ymax
    grow = (bw * h / w - bh) / 2
    return xmin, ymin - grow, xmax, ymax + grow


def parse_point(geometry: str | None, default: CRS) -> tuple[float, float, CRS]:
    """An identify point, as JSON {x, y, spatialReference} or as "x,y"."""
    try:
        if geometry and geometry.strip().startswith("{"):
            d = json.loads(geometry)
            sr = d.get("spatialReference")
            crs = parse_sr(json.dumps(sr), default) if sr else default
            x, y = float(d["x"]), float(d["y"])
        else:
            crs = default
            x, y = (float(p) for p in (geometry or "").split(","))
    except (ValueError, KeyError, TypeError):
        raise EsriError(400, "Unable to complete operation.", ["Invalid geometry"])
    if not (math.isfinite(x) and math.isfinite(y)):
        raise EsriError(400, "Unable to complete operation.", ["Invalid geometry"])
    return x, y, crs


def parse_quality(value: str | None) -> int:
    """compressionQuality, 1 to 100; 75 when it is not a number."""
    try:
        return min(max(int(float(value)), 1), 100)
    except (TypeError, ValueError, OverflowError):
        return 75


def extent(box, crs: CRS) -> dict:
    return {
        "xmin": box[0],
        "ymin": box[1],
        "xmax": box[2],
        "ymax": box[3],
        "spatialReference": spatial_reference(crs),
    }


def service(layer: Layer, max_image: int) -> dict:
    """The service description (`ImageServer?f=json`). Tiles are offered up to the COG's zoom."""
    o = copy.deepcopy(DEFAULTS)
    o["serviceDescription"] = o["description"] = layer.description
    o["name"] = layer.name
    o["copyrightText"] = layer.copyright
    for k in ("extent", "initialExtent", "fullExtent"):
        o[k] = extent(layer.extent, WEB_MERCATOR)
    o["pixelSizeX"] = o["pixelSizeY"] = layer.pixel_size
    o["bandNames"] = layer.band_names
    o["bandCount"] = len(layer.band_names)
    o["pixelType"] = PIXEL_TYPES.get(layer.dtype, "UNKNOWN")
    o["blockWidth"], o["blockHeight"] = layer.block
    o["compressionType"] = layer.compression
    itemsize = {"U8": 1, "S8": 1, "U16": 2, "S16": 2}.get(o["pixelType"], 4)
    o["uncompressedSize"] = layer.width * layer.height * len(layer.band_names) * itemsize
    if layer.dtype != "uint8":
        o["serviceDataType"] = "esriImageServiceDataTypeGeneric"
    stats = layer.stats or [{"min": 0, "max": 255, "mean": 0, "std": 0}] * len(layer.band_names)
    for key, s in (("minValues", "min"), ("maxValues", "max"), ("meanValues", "mean"), ("stdvValues", "std")):
        o[key] = [b[s] for b in stats]
    o["maxImageWidth"] = o["maxImageHeight"] = max_image
    lods = [lod for lod in o["tileInfo"]["lods"] if lod["level"] <= layer.maxzoom]
    o["tileInfo"]["lods"] = lods
    o["minLOD"], o["maxLOD"] = 0, lods[-1]["level"]
    o["minPixelSize"], o["maxPixelSize"] = lods[-1]["resolution"], lods[0]["resolution"]
    return o


def key_properties(layer: Layer) -> dict:
    data_type = "Processed" if layer.dtype == "uint8" else "Generic"
    return {"DataType": data_type, "BandProperties": [{"BandName": b} for b in layer.band_names]}


RASTER_FUNCTIONS = {
    "rasterFunctionInfos": [
        {"name": "None", "description": "", "help": "", "functionType": 0, "thumbnail": ""}
    ]
}


def legend(layer: Layer) -> dict:
    """The layer, without swatches: Map Viewer draws the legend of an RGB layer itself."""
    return {
        "layers": [
            {
                "layerId": 0,
                "layerName": layer.name,
                "layerType": "Raster Layer",
                "minScale": 0,
                "maxScale": 0,
                "legendType": "RGB Composite" if len(layer.band_names) >= 3 else "Stretched",
                "legend": [],
            }
        ]
    }
