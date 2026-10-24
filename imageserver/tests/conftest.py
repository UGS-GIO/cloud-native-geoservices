"""Small COGs made per test session, an items.json that lists them, and a client of the app."""

from __future__ import annotations

import json
import warnings

import numpy
import pytest
import rasterio
from fastapi.testclient import TestClient
from rasterio.transform import from_origin
from rasterio.warp import transform_bounds

# 512 x 512 pixels of 2 m in Web Mercator near Salt Lake City; the left quarter is transparent.
ORIGIN = (-12455000.0, 4980000.0)
SIZE, PIXEL = 512, 2.0


def pattern(bands: int, size: int) -> numpy.ndarray:
    y, x = numpy.mgrid[0:size, 0:size]
    return numpy.stack([(x + 7 * b) % 256 for b in range(bands - 1)] + [y % 256]).astype("uint8")


def write_cog(path, data, crs, transform, nodata=None, alpha=None):
    profile = {
        "driver": "COG",
        "count": data.shape[0],
        "height": data.shape[1],
        "width": data.shape[2],
        "dtype": data.dtype,
        "crs": crs,
        "transform": transform,
        "compress": "DEFLATE",
        "blocksize": 256,
        "nodata": nodata,
        "overview_resampling": "average",
    }
    if alpha is not None:
        data = numpy.concatenate([data, alpha[None]])
        profile["count"] += 1
    with rasterio.open(path, "w", **profile) as dst:
        dst.write(data)
        if alpha is not None:
            dst.colorinterp = [
                rasterio.enums.ColorInterp.red,
                rasterio.enums.ColorInterp.green,
                rasterio.enums.ColorInterp.blue,
                rasterio.enums.ColorInterp.alpha,
            ]


@pytest.fixture(scope="session")
def data_dir(tmp_path_factory):
    d = tmp_path_factory.mktemp("cogs")
    rgb = pattern(3, SIZE)
    alpha = numpy.full((SIZE, SIZE), 255, "uint8")
    alpha[:, : SIZE // 4] = 0
    write_cog(d / "rgba.tif", rgb, "EPSG:3857", from_origin(*ORIGIN, PIXEL, PIXEL), alpha=alpha)
    write_cog(d / "utm.tif", rgb, "EPSG:26912", from_origin(420000, 4510000, 2, 2))
    write_cog(
        d / "dem.tif", pattern(2, SIZE)[:1].astype("float32"), "EPSG:3857", from_origin(*ORIGIN, PIXEL, PIXEL)
    )

    def item(id, href, key="visual"):
        with rasterio.open(d / href) as ds:
            bbox = list(transform_bounds(ds.crs, "EPSG:4326", *ds.bounds))
        return {
            "id": id,
            "bbox": bbox,
            "properties": {"title": id.upper(), "datetime": "2024-01-01T00:00:00Z"},
            "assets": {
                key: {"href": href, "type": "image/tiff; application=geotiff; profile=cloud-optimized"}
            },
        }

    items = {
        "type": "FeatureCollection",
        "items": [
            item("rgba", "rgba.tif"),
            item("utm", "utm.tif", key="cog"),
            item("dem", "dem.tif"),
            {"id": "pdf-only", "assets": {"publication": {"href": "x.pdf", "type": "application/pdf"}}},
        ],
    }
    (d / "items.json").write_text(json.dumps(items))
    return d


@pytest.fixture(scope="session")
def client(data_dir):
    mp = pytest.MonkeyPatch()
    mp.setenv("STAC_COLLECTION_URL", str(data_dir / "items.json"))
    from imageserver.main import create_app

    with TestClient(create_app()) as c:
        yield c
    mp.undo()


def decode(body: bytes) -> numpy.ndarray:
    """An image response as a (bands, rows, cols) array."""
    with warnings.catch_warnings(), rasterio.MemoryFile(body) as mem:
        warnings.simplefilter("ignore", rasterio.errors.NotGeoreferencedWarning)
        with mem.open() as ds:
            return ds.read()
