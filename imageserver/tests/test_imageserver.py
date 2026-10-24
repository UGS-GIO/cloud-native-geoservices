from __future__ import annotations

import json

import pytest
from conftest import ORIGIN, PIXEL, SIZE, decode

S = "/ugs/rest/services"
X0, Y1 = ORIGIN
X1, Y0 = X0 + SIZE * PIXEL, Y1 - SIZE * PIXEL


def export(client, layer="rgba", **params):
    p = {"bbox": f"{X0},{Y0},{X1},{Y1}", "size": "64,64", "format": "png", "f": "image", **params}
    return client.get(f"{S}/{layer}/ImageServer/exportImage", params=p)


def test_root_lists_cog_items(client):
    services = client.get("/").json()["services"]
    assert [s["id"] for s in services] == ["rgba", "utm", "dem"]
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
        ({"size": "99999,10"}, "Requested image exceeds the size limit of 4100 x 4100"),
        ({"bboxSR": "999999"}, "Unknown spatial reference: 999999"),
    ],
)
def test_export_errors(client, params, detail):
    err = export(client, f="json", **params).json()["error"]
    assert err["code"] == 400 and err["details"] == [detail]


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


def test_cache_headers(client):
    res = export(client)
    assert res.headers["cache-control"].startswith("public, max-age=300")
    again = client.get(res.url, headers={"If-None-Match": res.headers["etag"]})
    assert again.status_code == 304
    assert export(client, bbox="").headers["cache-control"] == "no-store"


def test_key_properties_and_functions(client):
    assert client.get(f"{S}/rgba/ImageServer/keyProperties").json()["DataType"] == "Processed"
    names = [
        f["name"]
        for f in client.get(f"{S}/rgba/ImageServer/rasterFunctionInfos").json()["rasterFunctionInfos"]
    ]
    assert names == ["None"]
