"""User library of on-demand custom nodes and models.

Every wake is a fresh Colab VM, so nothing installed through ComfyUI-Manager survives a stop.
The library keeps the *list* (STATE_DIR/library.json, written by owners through /_gate/library)
and installs entries onto the running VM only when asked (POST /_gate/library/install) or,
for entries flagged ``auto_wake``, as part of every boot. Installation reuses the on-demand
fetch path of setup.py (parallel downloader, custom-node install); new custom nodes trigger a
ComfyUI restart so they register without a new wake.
"""
from __future__ import annotations

import json
import re
import urllib.request
from pathlib import Path
from typing import Any

from .profiles import hf_to_https

KINDS = ("node", "model")
_SLUG = re.compile(r"[^a-z0-9_-]+")
_DEST = re.compile(r"^[A-Za-z0-9_.\- ]+(/[A-Za-z0-9_.\- ]+)+$")
_SECRET = re.compile(r"(hf_[A-Za-z0-9]{20,}|ghp_[A-Za-z0-9]{20,}|token=|access_token|ya29\.)", re.I)


class LibraryError(ValueError):
    pass


def _path(state_dir: str) -> Path:
    return Path(state_dir) / "library.json"


def load(state_dir: str) -> list[dict]:
    p = _path(state_dir)
    if not p.is_file():
        return []
    try:
        d = json.loads(p.read_text())
    except ValueError:
        return []
    return [i for i in d.get("items", []) if isinstance(i, dict) and i.get("id")]


def save(state_dir: str, items: list[dict]) -> None:
    p = _path(state_dir)
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_suffix(".tmp")
    tmp.write_text(json.dumps({"items": items}, ensure_ascii=False, indent=1))
    tmp.replace(p)


def slug(s: str) -> str:
    return _SLUG.sub("-", s.lower()).strip("-")[:48] or "item"


def normalize(raw: dict[str, Any]) -> dict:
    """Validate one entry coming from a client. Raises LibraryError with a readable message."""
    kind = str(raw.get("kind", "")).strip()
    if kind not in KINDS:
        raise LibraryError("kind must be 'node' or 'model'")
    url = str(raw.get("url", "")).strip()
    if not (url.startswith("https://") or url.startswith("hf://")):
        raise LibraryError("url must start with https:// or hf://")
    if _SECRET.search(url):
        raise LibraryError("url looks like it contains a credential; use a public URL")
    title = str(raw.get("title") or "").strip()[:80]
    item: dict[str, Any] = {"kind": kind, "url": url, "auto_wake": bool(raw.get("auto_wake"))}
    if kind == "node":
        base = url.rstrip("/").rsplit("/", 1)[-1].removesuffix(".git")
        item["title"] = title or base
        item["id"] = slug(str(raw.get("id") or base))
        for k in ("ref", "subdir"):
            v = str(raw.get(k) or "").strip()
            if v:
                if ".." in v or v.startswith("/"):
                    raise LibraryError(f"bad {k}")
                item[k] = v
    else:
        dest = str(raw.get("dest", "")).strip().strip("/")
        if not _DEST.match(dest) or ".." in dest:
            raise LibraryError("dest must look like 'folder/file.safetensors' (relative to ComfyUI/models)")
        item["dest"] = dest
        item["title"] = title or dest.rsplit("/", 1)[-1]
        item["id"] = slug(str(raw.get("id") or dest.replace("/", "-")))
        size = raw.get("size_gb")
        if isinstance(size, (int, float)) and size > 0:
            item["size_gb"] = round(float(size), 3)
        sha = str(raw.get("sha256") or "").strip().lower()
        if re.fullmatch(r"[0-9a-f]{64}", sha):
            item["sha256"] = sha
    lic = str(raw.get("license") or "").strip()[:40]
    if lic:
        item["license"] = lic
    note = str(raw.get("note") or "").strip()[:200]
    if note:
        item["note"] = note
    return item


def probe_size_gb(url: str, timeout: float = 10) -> float | None:
    """Best effort: HEAD the (redirect-following) URL for Content-Length."""
    try:
        req = urllib.request.Request(hf_to_https(url), method="HEAD", headers={"User-Agent": "comfy-gate"})
        with urllib.request.urlopen(req, timeout=timeout) as r:  # noqa: S310 (user-supplied public URL)
            n = int(r.headers.get("Content-Length") or 0)
            return round(n / 1e9, 3) if n > 0 else None
    except Exception:  # noqa: BLE001
        return None


def upsert(items: list[dict], item: dict) -> list[dict]:
    out = [i for i in items if i["id"] != item["id"]]
    out.append(item)
    return out


def _model_entry(it: dict) -> dict:
    return {"id": "lib-" + it["id"], "dest": it["dest"], "url": hf_to_https(it["url"]), "sha256": it.get("sha256"),
            "size_gb": it.get("size_gb") or 0, "core": False}


def _node_entry(it: dict) -> dict:
    return {"id": it["id"], "url": it["url"], "ref": it.get("ref"), "subdir": it.get("subdir")}


def fetch_plan(base_plan: dict, items: list[dict]) -> dict:
    """On-demand install plan for a running VM (setup.py fetch_only path)."""
    plan = dict(base_plan)
    plan["models"] = [_model_entry(i) for i in items if i["kind"] == "model"]
    plan["custom_nodes"] = [_node_entry(i) for i in items if i["kind"] == "node"]
    plan["workflows"] = []
    plan["fetch_only"] = True
    plan["smoke"] = None
    plan["library_ids"] = [i["id"] for i in items]
    plan["restart_comfy"] = any(i["kind"] == "node" for i in items)
    return plan


def merge_into_boot(plan: dict, items: list[dict]) -> list[str]:
    """Add auto_wake entries to a boot plan (nodes before ComfyUI starts, models in the background)."""
    have_nodes = {n["id"] for n in plan.get("custom_nodes", [])}
    have_models = {m["dest"] for m in plan.get("models", [])}
    added = []
    for it in items:
        if it["kind"] == "node" and it["id"] not in have_nodes:
            plan.setdefault("custom_nodes", []).append(_node_entry(it))
        elif it["kind"] == "model" and it["dest"] not in have_models:
            plan.setdefault("models", []).append(_model_entry(it))
        else:
            continue
        added.append(it["id"])
    return added
