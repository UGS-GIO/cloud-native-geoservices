"""Record reference ArcGIS ImageServer answers to the request set (docs/imageserver.md).

    python imageserver/scripts/record.py                   # every layer
    python imageserver/scripts/record.py -only toronto     # one layer

Writes testdata/conformance/imageserver/<layer>/responses.json, one response per line, in the
layout cmd/conformance-record writes. Images are kept as base64 in `image`, other non-JSON
bodies in `text`.
"""

from __future__ import annotations

import argparse
import base64
import json
import math
import time
from datetime import UTC, datetime
from pathlib import Path
from urllib.parse import urlencode

import httpx

ROOT = Path(__file__).resolve().parents[2] / "testdata" / "conformance" / "imageserver"
USER_AGENT = "cloud-native-geoservices-conformance (+https://github.com/UGS-GIO/cloud-native-geoservices)"
R = 6378137.0


def mercator(lon: float, lat: float) -> tuple[float, float]:
    return R * math.radians(lon), R * math.log(math.tan(math.pi / 4 + math.radians(lat) / 2))


def tile(lon: float, lat: float, z: int) -> str:
    n = 2**z
    col = int((lon + 180) / 360 * n)
    row = int((1 - math.asinh(math.tan(math.radians(lat))) / math.pi) / 2 * n)
    return f"{z}/{row}/{col}"


def derived(vars: dict[str, str]) -> dict[str, str]:
    """The env (lon/lat) in Web Mercator, its center as JSON, and the tile holding the center."""
    out = dict(vars)
    x0, y0, x1, y1 = (float(v) for v in vars["env"].split(","))
    mx0, my0 = mercator(x0, y0)
    mx1, my1 = mercator(x1, y1)
    out["env3857"] = f"{mx0:.4f},{my0:.4f},{mx1:.4f},{my1:.4f}"
    lon, lat = (x0 + x1) / 2, (y0 + y1) / 2
    mx, my = mercator(lon, lat)
    out["ptjson"] = json.dumps({"x": round(mx, 4), "y": round(my, 4), "spatialReference": {"wkid": 102100}})
    out["pt4326json"] = json.dumps({"x": lon, "y": lat, "spatialReference": {"wkid": 4326}})
    out["tile"] = tile(lon, lat, int(vars["z"]))
    return out


def fill(s: str, vars: dict[str, str]) -> str:
    for k, v in vars.items():
        s = s.replace("{" + k + "}", v)
    return s


def get(client: httpx.Client, url: str) -> httpx.Response:
    for attempt in range(6):
        if attempt:
            time.sleep(2**attempt)
        try:
            res = client.get(url)
        except httpx.HTTPError:
            continue
        if res.status_code != 429 and res.status_code < 500:
            return res
    return res


def record(client: httpx.Client, layer: dict, requests: list[dict], gap: float) -> None:
    vars = derived(layer["vars"])
    responses = []
    for tmpl in requests:
        op = fill(tmpl["op"], vars)
        params = {k: fill(v, vars) for k, v in tmpl["params"].items()}
        url = layer["url"] + ("/" + op if op else "") + ("?" + urlencode(params) if params else "")
        time.sleep(gap)
        start = time.monotonic()
        res = get(client, url)
        ms = int((time.monotonic() - start) * 1000)
        ct = res.headers.get("content-type", "")
        r = {"name": tmpl["name"], "op": op, "params": params, "status": res.status_code, "contentType": ct}
        if ct.startswith("image/"):
            r["image"] = base64.b64encode(res.content).decode()
        else:
            try:
                r["body"] = json.loads(res.content)
            except ValueError:
                r["text"] = res.text
        r["ms"] = ms
        print(f"{layer['name']}/{tmpl['name']}: {res.status_code} {ct} {len(res.content)} bytes {ms} ms")
        responses.append(r)
    head = {"layer": layer, "recorded": datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")}
    lines = ",\n".join(json.dumps(r, separators=(",", ":")) for r in responses)
    out = ROOT / layer["name"] / "responses.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(head, separators=(",", ":"))[:-1] + ',"responses":[\n' + lines + "\n]}\n")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("-only", default="", help="comma-separated layer names")
    ap.add_argument("-gap", type=float, default=0.25, help="seconds between requests")
    args = ap.parse_args()
    layers = json.loads((ROOT / "layers.json").read_text())["layers"]
    requests = json.loads((ROOT / "requests.json").read_text())
    with httpx.Client(headers={"User-Agent": USER_AGENT}, timeout=120, follow_redirects=True) as client:
        for layer in layers:
            if not args.only or layer["name"] in args.only.split(","):
                record(client, layer, requests, args.gap)


if __name__ == "__main__":
    main()
