"""The rasters served: STAC items with a COG asset, from one or more ItemCollections (items.json),
read again on the first request after TTL. A refresh that fails keeps the last good list, as
internal/catalog does."""

from __future__ import annotations

import hashlib
import json
import os
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import quote, urljoin

import httpx

COG_TYPES = ("image/tiff",)  # every GeoTIFF media type starts with this
STALE_RETRY = 10.0  # seconds before a failed refresh is tried again


@dataclass
class Entry:
    item: dict  # the STAC item, with absolute asset hrefs
    url: str  # where the item lives, for STACReader
    asset: str  # the COG asset served
    version: str  # changes whenever the item's JSON does


def with_query(href: str, key: str, value: str) -> str:
    """href with ?key=value added, for http(s) hrefs only."""
    if not href.startswith(("http:", "https:")):
        return href
    return f"{href}{'&' if '?' in href else '?'}{key}={quote(value, safe='')}"


def _get(url: str, client: httpx.Client) -> dict:
    if url.startswith(("http:", "https:")):
        # _cb makes a CDN in front of the URL send the current file, not a cached copy.
        res = client.get(with_query(url, "_cb", str(int(time.time()))))
        res.raise_for_status()
        return res.json()
    return json.loads(Path(url).read_text())


def _absolute(base: str, href: str) -> str:
    if href.startswith(("http:", "https:", "/")):
        return href
    if base.startswith(("http:", "https:")):
        return urljoin(base, href)
    return str(Path(base).parent / href)


def parse(raw: dict, base: str, asset_keys: list[str]) -> list[Entry]:
    """Items that have one of asset_keys (first match wins) as a GeoTIFF."""
    out = []
    for it in raw.get("items") or raw.get("features") or []:
        assets = it.get("assets") or {}
        key = next(
            (
                k
                for k in asset_keys
                if k in assets and (assets[k].get("type") or "image/tiff").startswith(COG_TYPES)
            ),
            None,
        )
        if not it.get("id") or key is None:
            continue
        it = {"type": "Feature", "stac_version": "1.1.0", "geometry": None, "links": [], **it}
        it["assets"] = {k: {**a, "href": _absolute(base, a["href"])} for k, a in assets.items()}
        self_href = next((lk["href"] for lk in it["links"] if lk.get("rel") == "self"), it["id"] + ".json")
        version = hashlib.sha256(json.dumps(it, sort_keys=True).encode()).hexdigest()[:16]
        out.append(Entry(it, _absolute(base, self_href), key, version))
    return out


class Catalog:
    """Reads the list again on the first request after TTL. Other requests wait for that read."""

    def __init__(self, urls: list[str], asset_keys: list[str], ttl: float):
        self.urls, self.asset_keys, self.ttl = urls, asset_keys, ttl
        self._lock = threading.Lock()
        self._entries: dict[str, Entry] | None = None
        self._at = 0.0
        self._client = httpx.Client(timeout=10)

    def _load(self) -> dict[str, Entry]:
        with ThreadPoolExecutor(8) as pool:
            raws = list(pool.map(lambda u: _get(u, self._client), self.urls))
        entries: dict[str, Entry] = {}
        for url, raw in zip(self.urls, raws):
            for e in parse(raw, url, self.asset_keys):
                entries[e.item["id"]] = e  # the last of an id wins
        return entries

    def entries(self) -> dict[str, Entry]:
        with self._lock:
            if self._entries is None:
                self._entries, self._at = self._load(), time.monotonic()
            elif time.monotonic() - self._at >= self.ttl:
                try:
                    self._entries, self._at = self._load(), time.monotonic()
                except (httpx.HTTPError, OSError, ValueError):
                    self._at = time.monotonic() - self.ttl + STALE_RETRY
            return self._entries

    def get(self, id: str) -> Entry | None:
        return self.entries().get(id)

    def cog_version(self, entry: Entry) -> str | None:
        """The COG's ETag, or Last-Modified and size: it changes when the COG is rewritten in place.
        None when the COG cannot be reached."""
        href = entry.item["assets"][entry.asset]["href"]
        try:
            if not href.startswith(("http:", "https:")):
                st = os.stat(href)
                return f"{st.st_mtime_ns}-{st.st_size}"
            res = self._client.head(
                with_query(href, "_cb", str(int(time.time()))), follow_redirects=True
            ).raise_for_status()
        except (httpx.HTTPError, OSError):
            return None
        h = res.headers
        return h.get("etag") or f"{h.get('last-modified', '')}-{h.get('content-length', '')}"
