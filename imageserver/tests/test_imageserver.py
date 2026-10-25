from __future__ import annotations

import json
import os
import shutil
import threading
import time
from pathlib import Path

import httpx
import pytest
import rio_tiler.reader
from conftest import ORIGIN, PIXEL, SIZE, decode
from fastapi.testclient import TestClient

from imageserver import factory
from imageserver.catalog import Catalog, Entry
from imageserver.factory import ImageServerFactory

S = "/ugs/rest/services"
X0, Y1 = ORIGIN
X1, Y0 = X0 + SIZE * PIXEL, Y1 - SIZE * PIXEL


def export(client, layer="rgba", **params):
    p = {"bbox": f"{X0},{Y0},{X1},{Y1}", "size": "64,64", "format": "png", "f": "image", **params}
    return client.get(f"{S}/{layer}/ImageServer/exportImage", params=p)


def test_root_lists_cog_items(client):
    services = client.get("/").json()["services"]
    assert [s["id"] for s in services] == ["rgba", "nodata", "utm", "dem", "flat", "ovr"]
    assert services[0]["url"].endswith("/ugs/rest/services/rgba/ImageServer")


def test_service(client):
    d = client.get(f"{S}/rgba/ImageServer", params={"f": "json"}).json()
    assert d["extent"] == {
        "xmin": X0,
        "ymin": Y0,
        "xmax": X1,
        "ymax": Y1,
        "spatialReference": {"wkid": 102100, "latestWkid": 3857},
    }
    assert d["bandNames"] == ["Red", "Green", "Blue"] and d["bandCount"] == 3
    assert d["pixelType"] == "U8" and d["pixelSizeX"] == PIXEL
    assert d["compressionType"] == "Deflate" and (d["blockWidth"], d["blockHeight"]) == (256, 256)
    assert d["capabilities"] == "Image,Metadata"
    # 2 m pixels: tiles to level 17 (1.19 m), so no detail is lost.
    assert d["tileInfo"]["lods"][-1]["level"] == 17 == d["maxLOD"]
    assert d["cacheType"] == "Map" and d["singleFusedMapCache"] is True
    assert not d["hasRasterAttributeTable"] and not d["hasColormap"] and not d["hasHistograms"]


def test_service_in_another_crs(client):
    d = client.get(f"{S}/utm/ImageServer", params={"f": "json"}).json()
    assert d["spatialReference"]["wkid"] == 102100
    assert -12500000 < d["extent"]["xmin"] < d["extent"]["xmax"] < -12400000


def test_unknown_service(client):
    assert client.get(f"{S}/nope/ImageServer").json()["error"]["code"] == 404
    assert client.get(f"{S}/pdf-only/ImageServer").json()["error"]["code"] == 404


def test_export_png_nearest_is_exact(client):
    res = export(client, size="512,512", interpolation="RSP_NearestNeighbor")
    assert res.headers["content-type"] == "image/png"
    a = decode(res.content)
    assert a.shape == (4, 512, 512)
    assert (a[3, :, : SIZE // 4] == 0).all() and (a[3, :, SIZE // 4 + 1 :] == 255).all()
    assert a[0, 10, 300] == 300 % 256 and a[1, 10, 300] == 307 % 256 and a[2, 10, 300] == 10


def test_export_nodata_is_transparent(client):
    a = decode(export(client, layer="nodata", size="512,512", interpolation="RSP_NearestNeighbor").content)
    assert (a[3, :, : SIZE // 4] == 0).all() and (a[3, :, SIZE // 4 + 1 :] == 255).all()
    assert a[0, 10, 300] == 300 % 256
    ident = f"{S}/nodata/ImageServer/identify"
    assert client.get(ident, params={"geometry": f"{X0 + 3},{Y1 - 3}"}).json()["value"] == "NoData"
    assert client.get(ident, params={"geometry": f"{X0 + 601},{Y1 - 21}"}).json()["value"] == "44, 51, 10"


def test_post(client):
    form = {"bbox": f"{X0},{Y0},{X1},{Y1}", "size": "64,64", "format": "png", "f": "image"}
    res = client.post(f"{S}/rgba/ImageServer/exportImage", data=form)
    assert res.headers["content-type"] == "image/png" and decode(res.content).shape == (4, 64, 64)
    pt = {"geometry": f"{X0 + 601},{Y1 - 21}", "f": "json"}
    assert client.post(f"{S}/rgba/ImageServer/identify", data=pt).json()["value"] == "44, 51, 10"


def test_export_formats(client):
    assert export(client, format="jpgpng").headers["content-type"] == "image/png"  # has transparency
    opaque = f"{X0 + SIZE},{Y0},{X1},{Y1}"
    assert export(client, format="jpgpng", bbox=opaque, size="32,64").headers["content-type"] == "image/jpeg"
    assert export(client, format="jpg").headers["content-type"] == "image/jpeg"
    assert export(client, format="nonsense").headers["content-type"] == "image/png"  # as jpgpng


def test_export_json_adjusts_aspect(client):
    d = export(client, f="json", size="128,64").json()
    assert (d["width"], d["height"]) == (128, 64)
    e = d["extent"]
    assert e["xmax"] - e["xmin"] == pytest.approx(2 * (e["ymax"] - e["ymin"]))
    assert e["ymin"] == Y0 and e["ymax"] == Y1
    assert "f=image" in d["href"] and "/exportImage?" in d["href"]


def test_export_4326_and_outside(client):
    a = decode(export(client, bbox="-111.89,40.75,-111.88,40.76", bboxSR="4326", imageSR="4326").content)
    assert a.shape == (4, 64, 64)
    a = decode(export(client, bbox="0,0,1000,1000").content)
    assert (a[3] == 0).all()


@pytest.mark.parametrize(
    "params, detail",
    [
        ({"bbox": ""}, "bbox must be specified"),
        ({"bbox": "1,2,3"}, "Invalid bbox"),
        ({"size": "2049,10"}, "Requested image exceeds the size limit of 2048 x 2048"),
        ({"size": "inf,10"}, "Invalid size"),
        ({"bbox": "nan,0,1,1"}, "Invalid bbox"),
        ({"bbox": "0,0,inf,1"}, "Invalid bbox"),
        ({"bboxSR": "999999"}, "Unknown spatial reference: 999999"),
    ],
)
def test_export_errors(client, params, detail):
    err = export(client, f="json", **params).json()["error"]
    assert err["code"] == 400 and err["details"] == [detail]


def test_export_huge_bbox(client):
    assert export(client, bbox="-1e10,-1e10,1e10,1e10", bboxSR="4326").json()["error"]["details"] == [
        "Invalid bbox"
    ]


def within(seconds, fn, *args, **kwargs):
    """fn's result, or a failure once it runs longer than seconds (a GDAL read can't be interrupted)."""
    out = []
    t = threading.Thread(target=lambda: out.append(fn(*args, **kwargs)), daemon=True)
    t.start()
    t.join(seconds)
    assert out, f"no answer in {seconds} s"
    return out[0]


@pytest.fixture
def vrt_pixels(monkeypatch):
    """The size of each warped VRT rio-tiler opens."""
    sizes, real = [], rio_tiler.reader.WarpedVRT

    def spy(src, **kw):
        sizes.append(kw.get("width", src.width) * kw.get("height", src.height))
        return real(src, **kw)

    monkeypatch.setattr(rio_tiler.reader, "WarpedVRT", spy)
    return sizes


@pytest.mark.parametrize("layer", ["nodata", "flat"])
@pytest.mark.parametrize(
    "path, params",
    [
        ("exportImage", {"bbox": "-180,-90,180,90", "bboxSR": "4326", "size": "400,400"}),
        ("exportImage", {"bbox": "-1e30,-1e30,1e30,1e30", "size": "400,400"}),
        ("tile/0/0/0", {}),
    ],
)
def test_world_reads_only_the_cog(client, vrt_pixels, layer, path, params):
    p = {"f": "image", "format": "png", **params}
    res = within(2, client.get, f"{S}/{layer}/ImageServer/{path}", params=p)
    assert res.status_code == 200
    assert decode(res.content).shape[1:] == ((400, 400) if params else (256, 256))
    assert max(vrt_pixels, default=0) <= SIZE * SIZE
    assert client.get(f"{S}/{layer}/ImageServer/{path}", params=p).content == res.content


def test_read_past_the_edge_uses_overviews(client):
    # A box past ovr.tif's edges at 1/8 its resolution: the full resolution averages its stripes.
    m = 256
    res = within(2, export, client, "ovr", bbox=f"{X0 - m},{Y0 - m},{X1 + m},{Y1 + m}", size="192,192")
    a = decode(res.content)
    assert a[0, 96, 96] in (0, 200) and a[3, 0, 0] == 0


def test_no_overviews_caps_the_read(data_dir, monkeypatch):
    monkeypatch.setenv("STAC_COLLECTION_URL", str(data_dir / "items.json"))
    monkeypatch.setenv("MAX_IMAGE_SIZE", "64")
    from imageserver.main import create_app

    c = TestClient(create_app())
    for params in ({}, {"bbox": "-180,-90,180,90", "bboxSR": "4326"}):
        err = within(2, export, c, "flat", **params).json()["error"]
        assert err["details"] == ["The raster has no overviews for this scale"]
    assert export(c, "nodata").headers["content-type"] == "image/png"
    assert export(c, "flat", size="64,64", bbox=f"{X0},{Y1 - 256},{X0 + 256},{Y1}").status_code == 200


def test_size_limit_in_service(client):
    d = client.get(f"{S}/rgba/ImageServer", params={"f": "json"}).json()
    assert d["maxImageWidth"] == d["maxImageHeight"] == 2048


def test_compression_quality_not_a_number(client):
    assert export(client, format="jpg", compressionQuality="abc").headers["content-type"] == "image/jpeg"


def test_export_unsupported_pixel_type(client):
    assert export(client, layer="dem").json()["error"]["details"] == ["Pixel type float32 is not supported"]


def test_identify(client):
    pt = {"x": X0 + 300 * PIXEL + 1, "y": Y1 - 10 * PIXEL - 1}
    d = client.get(
        f"{S}/rgba/ImageServer/identify",
        params={"geometry": json.dumps(pt), "geometryType": "esriGeometryPoint", "f": "json"},
    ).json()
    assert d["value"] == f"{300 % 256}, {307 % 256}, 10"
    assert d["location"]["x"] == pt["x"] and d["location"]["spatialReference"]["wkid"] == 102100
    transparent = json.dumps({"x": X0 + 3, "y": Y1 - 3})
    assert (
        client.get(f"{S}/rgba/ImageServer/identify", params={"geometry": transparent}).json()["value"]
        == "NoData"
    )
    outside = json.dumps({"x": 0, "y": 0, "spatialReference": {"wkid": 4326}})
    d = client.get(f"{S}/rgba/ImageServer/identify", params={"geometry": outside}).json()
    assert d["value"] == "NoData" and d["location"]["x"] == 0


def test_tile(client):
    # The level 14 tile that holds the raster's center.
    res = client.get(f"{S}/rgba/ImageServer/tile/14/6156/3100")
    assert res.status_code == 200 and res.headers["content-type"] in ("image/png", "image/jpeg")
    assert decode(res.content).shape[1:] == (256, 256)
    assert client.get(f"{S}/rgba/ImageServer/tile/14/0/0").status_code == 404
    assert client.get(f"{S}/rgba/ImageServer/tile/18/0/0").status_code == 404
    for bad in ("-1/0/0", "x/0/0", "3/-1/0", "3/0/8"):
        assert client.get(f"{S}/rgba/ImageServer/tile/{bad}").status_code == 404, bad


def test_unknown_operation_is_the_service(client):
    d = client.get(f"{S}/rgba/ImageServer/nonsense", params={"f": "json"}).json()
    assert d["name"] == "rgba" and "tileInfo" in d
    res = client.get(f"{S}/rgba/MapServer")
    assert res.json()["error"]["code"] == 404 and res.headers["cache-control"] == "no-store"


def test_cache_headers(client):
    res = export(client)
    assert res.headers["cache-control"].startswith("public, max-age=300, s-maxage=300,")
    again = client.get(res.url, headers={"If-None-Match": res.headers["etag"]})
    assert again.status_code == 304
    assert export(client, bbox="").headers["cache-control"] == "no-store"


def test_not_modified_before_reading(client, monkeypatch):
    res = export(client)
    url, tag = res.url, res.headers["etag"]

    def boom(*args):
        raise AssertionError("read the COG for a 304")

    monkeypatch.setattr(ImageServerFactory, "open", boom)
    for inm in (f"W/{tag}", f'"other", {tag}', "*"):
        res = client.get(url, headers={"If-None-Match": inm})
        assert res.status_code == 304, inm


@pytest.fixture
def fresh(data_dir, tmp_path, monkeypatch):
    """An app on a copy of rgba.tif that checks the COG on every request."""
    shutil.copy(data_dir / "rgba.tif", tmp_path / "rgba.tif")
    items = json.loads((data_dir / "items.json").read_text())
    items["items"] = items["items"][:1]
    (tmp_path / "items.json").write_text(json.dumps(items))
    monkeypatch.setenv("STAC_COLLECTION_URL", str(tmp_path / "items.json"))
    monkeypatch.setenv("CACHE_TTL", "0")
    from imageserver.main import create_app

    with TestClient(create_app()) as c:
        yield c, tmp_path / "rgba.tif"


def test_etag_changes_with_the_cog_and_the_build(fresh, data_dir, monkeypatch):
    c, cog = fresh
    url = f"{S}/rgba/ImageServer"
    tag = c.get(url).headers["etag"]
    assert c.get(url, headers={"If-None-Match": tag}).status_code == 304
    shutil.copy(data_dir / "nodata.tif", cog)  # rewritten in place, same href and item
    os.utime(cog, ns=(1, 1))
    res = c.get(url, headers={"If-None-Match": tag})
    assert res.status_code == 200 and res.headers["etag"] != tag
    tag = res.headers["etag"]
    monkeypatch.setattr(factory, "BUILD", "9.9.9")
    res = c.get(url, headers={"If-None-Match": tag})
    assert res.status_code == 200 and res.headers["etag"] != tag


def test_build_without_git_sha(tmp_path, monkeypatch):
    monkeypatch.delenv("GIT_SHA", raising=False)
    root = tmp_path / "imageserver"
    shutil.copytree(Path(factory.__file__).parent, root)
    build = factory._build(root)
    assert build == factory._build(root)
    assert factory._build(root, lambda d: "0") != build
    src = root / "esri.py"
    src.write_bytes(src.read_bytes() + b" ")
    assert factory._build(root) != build
    monkeypatch.setenv("GIT_SHA", "abc")
    assert factory._build(root) == "abc"


def test_etag_survives_a_failed_cog_check(fresh):
    c, cog = fresh
    url = f"{S}/rgba/ImageServer"
    tag = c.get(url).headers["etag"]
    cog.unlink()  # the HEAD fails
    res = c.get(url, headers={"If-None-Match": tag})
    assert res.status_code == 304


def test_busy_export_does_not_wait(data_dir, monkeypatch):
    monkeypatch.setenv("STAC_COLLECTION_URL", str(data_dir / "items.json"))
    monkeypatch.setenv("EXPORT_WAIT", "0.2")
    from imageserver.main import create_app

    c = TestClient(create_app())
    entered, release = threading.Event(), threading.Event()
    real = ImageServerFactory.open

    def hold(self, entry):
        if not entered.is_set():
            entered.set()
            release.wait(5)
        return real(self, entry)

    monkeypatch.setattr(ImageServerFactory, "open", hold)
    first = threading.Thread(target=export, args=(c,))
    first.start()
    try:
        assert entered.wait(5)
        threading.Timer(2, release.set).start()
        start = time.monotonic()
        res = export(c, size="32,32")
        assert time.monotonic() - start < 1
        assert res.json()["error"]["code"] == 503 and res.headers["cache-control"] == "no-store"
        assert c.get(f"{S}/rgba/ImageServer/tile/14/6156/3100").status_code == 200
    finally:
        release.set()
        first.join()


@pytest.mark.parametrize(
    "headers, version",
    [
        ({"ETag": '"abc"', "Last-Modified": "x"}, '"abc"'),
        (
            {"Last-Modified": "Tue, 01 Sep 2026 00:00:00 GMT", "Content-Length": "9"},
            "Tue, 01 Sep 2026 00:00:00 GMT-9",
        ),
    ],
)
def test_cog_version_from_head(headers, version):
    cat = Catalog([], [], 300)
    cat._client = httpx.Client(
        transport=httpx.MockTransport(lambda req: httpx.Response(200, headers=headers))
    )
    e = Entry({"assets": {"cog": {"href": "https://example.com/a.tif"}}}, "", "cog", "")
    assert cat.cog_version(e) == version


def test_catalog_last_url_wins(tmp_path):
    urls = []
    for i in range(3):
        f = tmp_path / f"{i}.json"
        f.write_text(json.dumps({"items": [{"id": "a", "assets": {"cog": {"href": f"{i}.tif"}}}]}))
        urls.append(str(f))
    assert Catalog(urls, ["cog"], 300).get("a").item["assets"]["cog"]["href"].endswith("2.tif")


def test_key_properties_and_functions(client):
    assert client.get(f"{S}/rgba/ImageServer/keyProperties").json()["DataType"] == "Processed"
    names = [
        f["name"]
        for f in client.get(f"{S}/rgba/ImageServer/rasterFunctionInfos").json()["rasterFunctionInfos"]
    ]
    assert names == ["None"]
