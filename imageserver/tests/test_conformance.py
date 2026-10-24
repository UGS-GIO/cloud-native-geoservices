"""Compares answers with the ArcGIS Server answers recorded in testdata/conformance/imageserver
(imageserver/scripts/record.py). The reference rasters are not copied, so the checks are on the
shape of each answer and on the parts that do not depend on the pixels."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from conftest import ORIGIN, PIXEL, SIZE

from imageserver import esri

ROOT = Path(__file__).resolve().parents[2] / "testdata" / "conformance" / "imageserver"
S = "/ugs/rest/services/rgba/ImageServer"
BBOX = f"{ORIGIN[0]},{ORIGIN[1] - SIZE * PIXEL},{ORIGIN[0] + SIZE * PIXEL},{ORIGIN[1]}"
# Service keys this server leaves out, with the reason.
KNOWN_GAPS: dict[str, str] = {}


def recorded(layer: str) -> dict[str, dict]:
    rec = json.loads((ROOT / layer / "responses.json").read_text())
    return {r["name"]: r for r in rec["responses"]}


def kind(v) -> str:
    return "number" if isinstance(v, (int, float)) and not isinstance(v, bool) else type(v).__name__


def test_service_has_every_key_of_a_dataset_and_a_cached_service(client):
    ours = client.get(S, params={"f": "json"}).json()
    dataset, cached = recorded("mtbaldy")["service"]["body"], recorded("toronto")["service"]["body"]
    expect = {k: v for k, v in dataset.items() if k not in KNOWN_GAPS}
    expect.update({k: cached[k] for k in ("tileInfo", "singleFusedMapCache", "cacheType", "storageInfo")})
    for k, v in expect.items():
        assert k in ours, k
        if v is not None:
            assert kind(ours[k]) == kind(v), k
    tiles, ref = ours["tileInfo"], cached["tileInfo"]
    assert {k: tiles[k] for k in ("rows", "cols", "dpi", "origin", "spatialReference")} == {
        k: ref[k] for k in ("rows", "cols", "dpi", "origin", "spatialReference")
    }
    assert tiles["format"] == ref["format"] == "Mixed"
    assert tiles["lods"] == ref["lods"][: len(tiles["lods"])]


@pytest.mark.parametrize("layer", ["toronto", "mtbaldy", "naip"])
@pytest.mark.parametrize("name", ["export-json", "export-4326-json"])
def test_export_extent_follows_the_aspect_ratio(layer, name):
    r = recorded(layer)[name]
    p, got = r["params"], r["body"]["extent"]
    w, h = (int(v) for v in p["size"].split(","))
    box = esri.adjust_aspect(esri.parse_bbox(p["bbox"]), w, h)
    assert box == pytest.approx((got["xmin"], got["ymin"], got["xmax"], got["ymax"]), rel=1e-9)


def test_export_json_shape(client):
    ref = recorded("toronto")["export-json"]["body"]
    ours = client.get(f"{S}/exportImage", params={"bbox": BBOX, "size": "64,64", "f": "json"}).json()
    assert list(ours) == list(ref) and list(ours["extent"]) == list(ref["extent"])


def test_export_errors_match(client):
    ref = recorded("toronto")["export-nobbox"]["body"]
    assert client.get(f"{S}/exportImage", params={"size": "64,64", "f": "json"}).json() == ref
    # An unknown format is jpgpng.
    assert recorded("toronto")["export-badformat"]["body"]["href"].endswith(".jpg")
    href = client.get(f"{S}/exportImage", params={"bbox": BBOX, "format": "xyz", "f": "json"}).json()["href"]
    assert "format=jpgpng" in href


@pytest.mark.parametrize("name", ["export-png", "export-jpgpng", "export-jpg", "export-4326"])
def test_export_content_type(client, name):
    r = recorded("toronto")[name]
    p = {**r["params"], "bbox": BBOX, "bboxSR": "102100", "imageSR": "102100"}
    ours = client.get(f"{S}/exportImage", params=p)
    assert ours.headers["content-type"].split(";")[0] in (r["contentType"], "image/png")


def test_identify_shape(client):
    ref = recorded("toronto")
    pt = json.dumps({"x": ORIGIN[0] + 900, "y": ORIGIN[1] - 100})
    ours = client.get(f"{S}/identify", params={"geometry": pt, "f": "json"}).json()
    assert list(ours) == list(ref["identify"]["body"])
    assert ours["value"].count(", ") == 2  # bands joined as "597, 651, 586, 577"
    outside = client.get(f"{S}/identify", params={"geometry": '{"x":0,"y":0}'}).json()
    assert outside["value"] == ref["identify-outside"]["body"]["value"] == "NoData"


def test_tile_outside_is_404(client):
    assert recorded("toronto")["tile-outside"]["status"] == 404
    assert client.get(f"{S}/tile/2/0/0").status_code == 404


def test_raster_functions_and_key_properties(client):
    ref = recorded("mtbaldy")
    assert client.get(f"{S}/rasterFunctionInfos").json() == ref["rasterFunctionInfos"]["body"]
    assert "DataType" in client.get(f"{S}/keyProperties").json()
    assert "DataType" in ref["keyProperties"]["body"]


@pytest.mark.parametrize("op", ["statistics", "histograms", "rasterAttributeTable"])
def test_empty_operations(client, op):
    # NAIP Plus has no statistics or histograms; no reference has an attribute table.
    ours = client.get(f"{S}/{op}", params={"f": "json"}).json()
    assert ours == recorded("naip")[op]["body"]
    assert list(ours) == list(recorded("mtbaldy")[op]["body"])


def test_legend_shape(client):
    ref = recorded("naip")["legend"]["body"]["layers"][0]
    ours = client.get(f"{S}/legend", params={"f": "json"}).json()["layers"]
    assert len(ours) == 1 and list(ours[0]) == list(ref)
    assert ours[0]["layerName"] == "rgba" and ours[0]["legendType"] == ref["legendType"]
