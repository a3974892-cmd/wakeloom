#!/usr/bin/env python3
"""Prepare verified installation assets on the existing gateway (no restart or GPU action).

Requires the bootstrap test6 and Ollama download fixes. Run on the VPS as root:
  python3 prepare-download-cache.py --container comfy-gate
Only official public packages are placed in the existing static directory.
Access credentials stay in a separate private file shipped to the user's Colab VM.
"""
import argparse
import ast
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import urllib.request
import urllib.parse

ASSETS = {
    "cloudflared": {
        "file": "cloudflared-2026.10.7-linux-amd64.deb", "size": 19925068,
        "sha256": "70b1a1e0f46033ffc8d90b406e554044ba6cbf2e4360295ef1163aed31e09393",
        "urls": ["https://api.github.com/repos/cloudflare/cloudflared/releases/assets/692817258",
                 "https://github.com/cloudflare/cloudflared/releases/download/2026.10.7/cloudflared-linux-amd64.deb"],
    },
    "ollama": {
        "file": "ollama-v0.40.15-linux-amd64.tar.zst", "size": 1543547172,
        "sha256": "628c018404a4206493efd38e46251c154ede5141b8b8dc85c659b3b027c47379",
        "urls": ["https://api.github.com/repos/ollama/ollama/releases/assets/715495290",
                 "https://github.com/ollama/ollama/releases/download/v0.40.15/ollama-linux-amd64.tar.zst",
                 "https://ollama.com/download/ollama-linux-amd64.tar.zst?version=0.40.15"],
    },
}
EXPECTED = {
    "_wb_bootstrap_download": "ee7afdd67de20d27f68543a0d765195d33f6e85449073b8a35d952cdef9dbb3d",
    "_ollama_install_archive": "88c17ac24e70a2b0db05ce564f9d337ca58f1e712726ad223da87dfdb1414cec",
}
START = "# wb-download-cache start\n"
END = "# wb-download-cache end\n"
PRIVATE_NAME = "wb-download-cache-private.json"

RUNTIME = r'''
# wb-download-cache start
def _wb_cached_package(key, destination, expected_size, expected_sha, note, cancel=None):
    import hashlib
    import json
    import os
    import time
    import urllib.request
    import urllib.parse
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    config = os.path.join(os.path.dirname(os.path.abspath(__file__)), "wb-download-cache-private.json")
    if not os.path.isfile(config):
        return False
    touched = False
    try:
        with open(config, encoding="utf-8") as f:
            cfg = json.load(f)
        entry = cfg["assets"][key]
        if entry["size"] != expected_size or entry["sha256"] != expected_sha:
            raise ValueError("cache version mismatch")
        base = urllib.parse.urlsplit(cfg["base_url"])
        if (base.scheme != "https" or not base.hostname or base.username or base.password
                or base.query or base.fragment or base.path != "/_gate/static/wb-installers/"):
            raise ValueError("invalid cache origin")
        filename = entry["file"]
        if not filename or filename != os.path.basename(filename) or "/" in filename or "\\" in filename:
            raise ValueError("invalid cache filename")
        headers = {k: v for k, v in cfg.get("headers", {}).items()
                   if k in ("CF-Access-Client-Id", "CF-Access-Client-Secret")}
        headers.update({"User-Agent": "Wakeloom/1.21.3", "Accept-Encoding": "identity"})
        opener = urllib.request.build_opener(NoRedirect())
        url = cfg["base_url"] + filename
        deadline, offset, next_note = time.monotonic() + 600, 0, 0
        note("using verified gateway cache")
        with open(destination, "wb") as out:
            touched = True
            while offset < expected_size:
                end = min(offset + 32 * 1024 * 1024, expected_size) - 1
                for attempt in range(3):
                    if cancel is not None and cancel.is_set():
                        raise RuntimeError("cache download canceled")
                    if time.monotonic() >= deadline:
                        raise TimeoutError("cache download timeout")
                    out.seek(offset)
                    out.truncate()
                    try:
                        request = urllib.request.Request(url, headers={**headers, "Range": f"bytes={offset}-{end}"})
                        with opener.open(request, timeout=30) as response:
                            wanted = f"bytes {offset}-{end}/{expected_size}"
                            if response.status != 206 or response.headers.get("Content-Range") != wanted:
                                raise ValueError("cache must return the exact requested byte range")
                            remaining = end - offset + 1
                            while remaining:
                                if cancel is not None and cancel.is_set():
                                    raise RuntimeError("cache download canceled")
                                if time.monotonic() >= deadline:
                                    raise TimeoutError("cache download timeout")
                                chunk = response.read(min(1024 * 1024, remaining))
                                if not chunk:
                                    raise IOError("incomplete cache range")
                                out.write(chunk)
                                remaining -= len(chunk)
                                if time.monotonic() >= next_note:
                                    note(f"cache {out.tell()}/{expected_size} bytes")
                                    next_note = time.monotonic() + 5
                            if response.read(1):
                                raise ValueError("oversized cache range")
                        break
                    except Exception:
                        if attempt == 2 or (cancel is not None and cancel.is_set()):
                            raise
                        note(f"cache range retry {attempt + 1}/2")
                offset = end + 1
        digest = hashlib.sha256()
        with open(destination, "rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        if os.path.getsize(destination) != expected_size or digest.hexdigest() != expected_sha:
            raise ValueError("cache package SHA-256 mismatch")
        note("gateway cache size and SHA-256 verified")
        return True
    except Exception as exc:
        if touched and os.path.isfile(destination):
            os.unlink(destination)
        if cancel is not None and cancel.is_set():
            raise RuntimeError("cache download canceled") from None
        # Never print credentials, signed URLs or response bodies.
        note(f"gateway cache unavailable ({type(exc).__name__}); using official download routes")
        return False


_wb_cache_original_boot_download = _wb_bootstrap_download
_wb_cache_original_ollama_archive = _ollama_install_archive


def _wb_bootstrap_download(urls, package, cancel, expected_size, expected_sha):
    note = lambda message: print("KIT_BOOTSTRAP cloudflared-download " + message, flush=True)
    if _wb_cached_package("cloudflared", package, expected_size, expected_sha, note, cancel):
        return
    return _wb_cache_original_boot_download(urls, package, cancel, expected_size, expected_sha)


def _ollama_install_archive(dest, note):
    if _wb_cached_package("ollama", dest, OLLAMA_INSTALL_SIZE, OLLAMA_INSTALL_SHA256, note):
        return
    return _wb_cache_original_ollama_archive(dest, note)
# wb-download-cache end
'''


def patch_source(source):
    text = source.replace("\r\n", "\n")
    if START in text or END in text:
        if text.count(START) != 1 or text.count(END) != 1:
            raise RuntimeError("Ambiguous cache markers; nothing changed")
        a, b = text.index(START), text.index(END) + len(END)
        if text[a:b] != RUNTIME.strip() + "\n":
            raise RuntimeError("Different cache patch detected; nothing changed")
        return source
    tree = ast.parse(text)
    for name, expected in EXPECTED.items():
        nodes = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == name]
        if len(nodes) != 1:
            raise RuntimeError("Install bootstrap test6 and the Ollama download patch first")
        # Python 3.13+ can omit empty fields; fingerprints use the complete
        # pre-3.13 form. Keep the same whitelist and reject changed code.
        options = {"show_empty": True} if sys.version_info >= (3, 13) else {}
        signature = ast.dump(nodes[0], include_attributes=False, **options).replace(", type_params=[]", "")
        if hashlib.sha256(signature.encode()).hexdigest() != expected:
            raise RuntimeError("Unrecognized download function; nothing changed")
    mains = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "main"]
    if len(mains) != 1:
        raise RuntimeError("Unrecognized main; nothing changed")
    lines = text.splitlines(keepends=True)
    offset = sum(len(s) for s in lines[:mains[0].lineno - 1])
    updated = text[:offset] + RUNTIME.strip() + "\n\n\n" + text[offset:]
    compile(updated, "setup.py", "exec")
    return updated.replace("\n", "\r\n") if "\r\n" in source else updated


def verified(path, asset):
    if not path.is_file() or path.stat().st_size != asset["size"]:
        return False
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest() == asset["sha256"]


def cache_asset(folder, asset):
    target = folder / asset["file"]
    if verified(target, asset):
        print("Cache already verified: " + asset["file"], flush=True)
        return target
    fd, temporary = tempfile.mkstemp(prefix=".wb-download-", dir=folder)
    os.close(fd)
    partial = Path(temporary)
    try:
        for url in asset["urls"]:
            print("Preparing " + asset["file"] + " from " + urllib.parse.urlsplit(url).hostname, flush=True)
            try:
                request = urllib.request.Request(url, headers={"User-Agent": "wakeloom-kit",
                    "Accept": "application/octet-stream", "Cache-Control": "no-cache"})
                start, next_note, size = time.monotonic(), time.monotonic() + 5, 0
                with urllib.request.urlopen(request, timeout=30) as response, partial.open("wb") as output:
                    if response.status != 200:
                        raise ValueError("not a complete release response")
                    while True:
                        block = response.read(1024 * 1024)
                        if not block:
                            break
                        size += len(block)
                        if size > asset["size"] or time.monotonic() - start > 900:
                            raise ValueError("release too large or download timed out")
                        output.write(block)
                        if time.monotonic() >= next_note:
                            print(f"  {size / 1e6:.0f}/{asset['size'] / 1e6:.0f} MB", flush=True)
                            next_note = time.monotonic() + 5
                if not verified(partial, asset):
                    raise ValueError("package size/SHA-256 mismatch")
                os.chmod(partial, 0o644)
                os.replace(partial, target)
                print("Verified: " + asset["file"], flush=True)
                return target
            except Exception as exc:
                print("  Download failed: " + type(exc).__name__ + " " + str(getattr(exc, "code", "")), flush=True)
        raise RuntimeError("Official download routes failed; startup code was not changed")
    finally:
        partial.unlink(missing_ok=True)


def container_info(container):
    code = '''import os,json
from pathlib import Path
from gate.config import Settings
from gate.proxy import service_headers
s=Settings.from_env()
p=Path(s.state_dir)/'session.json'
state=json.loads(p.read_text()).get('state') if p.is_file() else 'unknown'
print(json.dumps({'host':s.public_host,'headers':service_headers(s.cf_service_token_file),
 'state':state,'uid':os.geteuid(),'gid':os.getegid()}))
'''
    result = subprocess.run(["docker", "exec", "-i", container, "python", "-"], input=code,
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if result.returncode:
        raise RuntimeError("Cannot read gateway settings; nothing changed")
    return json.loads(result.stdout)


def mounted_dir(mounts, destination):
    for mount in sorted(mounts, key=lambda m: len(m.get("Destination", "")), reverse=True):
        prefix = mount.get("Destination", "").rstrip("/")
        if mount.get("Type") != "bind" or not prefix:
            continue
        if destination == prefix or destination.startswith(prefix + "/"):
            base = Path(mount["Source"]).resolve()
            target = (base / destination[len(prefix):].lstrip("/")).resolve()
            if target.is_relative_to(base) and target.is_dir():
                return target
    raise RuntimeError("Supported directory bind mount not found: " + destination)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def check_access(base_url, headers):
    opener = urllib.request.build_opener(NoRedirect())
    for asset in ASSETS.values():
        req = urllib.request.Request(base_url + asset["file"], headers={**headers,
            "User-Agent": "Wakeloom/1.21.3", "Accept-Encoding": "identity", "Range": "bytes=0-31"})
        with opener.open(req, timeout=30) as r:
            if r.status != 206 or r.headers.get("Content-Range") != f"bytes 0-31/{asset['size']}":
                raise RuntimeError("Gateway cache route does not serve exact file ranges")
            if len(r.read(32)) != 32:
                raise RuntimeError("Gateway cache response was incomplete")


def atomic_write(path, data, mode, uid, gid):
    fd, name = tempfile.mkstemp(prefix=".wb-cache-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(name, mode)
        if hasattr(os, "chown"):
            os.chown(name, uid, gid)
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--container", default="comfy-gate")
    parser.add_argument("--check", action="store_true", help="read-only preflight, no downloads or writes")
    args = parser.parse_args()
    if os.name != "posix" or os.geteuid() != 0:
        raise RuntimeError("Run on the VPS as root; no file was changed")
    meta = container_info(args.container)
    if meta["state"] != "sleeping":
        raise RuntimeError("Wait until the gateway is sleeping; this helper never stops a GPU")
    inspect = subprocess.run(["docker", "inspect", "--format", "{{json .Mounts}}", args.container],
                             text=True, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    mounts = json.loads(inspect.stdout)
    colab = mounted_dir(mounts, "/app/packages/colab")
    web = mounted_dir(mounts, "/app/apps/gate/web")
    if colab.is_relative_to(web) or web.is_relative_to(colab):
        raise RuntimeError("Private setup and public web directories must be separate")
    setup = colab / "setup.py"
    raw = setup.read_bytes()
    updated = patch_source(raw.decode("utf-8")).encode("utf-8")
    host = meta["host"]
    parsed = urllib.parse.urlsplit("https://" + host)
    if not parsed.hostname or parsed.username or parsed.password or parsed.path or parsed.query or parsed.fragment:
        raise RuntimeError("PUBLIC_HOST must be a hostname; nothing changed")
    base_url = "https://" + host + "/_gate/static/wb-installers/"
    headers = {k: v for k, v in meta["headers"].items()
               if k in ("CF-Access-Client-Id", "CF-Access-Client-Secret")}
    # Confirm existing credentials work for this gateway; do not weaken Access rules.
    health = urllib.request.Request("https://" + host + "/_gate/healthz",
                                    headers={**headers, "User-Agent": "Wakeloom/1.21.3"})
    with urllib.request.build_opener(NoRedirect()).open(health, timeout=20) as r:
        if not json.load(r).get("ok"):
            raise RuntimeError("Gateway health check failed")
    needed = sum(a["size"] for a in ASSETS.values()) * 2 + 100 * 1024 * 1024
    if shutil.disk_usage(web).free < needed:
        raise RuntimeError("Need at least 3.3 GB free disk for verified cache preparation")
    print("Preflight passed: known download functions, sleeping gateway, private config and existing HTTPS route")
    if args.check:
        print("Check only; no files changed, no packages downloaded")
        return
    cache = web / "wb-installers"
    if cache.is_symlink():
        raise RuntimeError("Cache directory must not be a symlink")
    cache.mkdir(mode=0o755, exist_ok=True)
    for asset in ASSETS.values():
        cache_asset(cache, asset)
    check_access(base_url, headers)
    if container_info(args.container)["state"] != "sleeping" or setup.read_bytes() != raw:
        raise RuntimeError("Gateway state or startup source changed; cache prepared but not activated")
    config = {"base_url": base_url, "headers": headers,
              "assets": {key: {k: asset[k] for k in ("file", "size", "sha256")} for key, asset in ASSETS.items()}}
    atomic_write(colab / PRIVATE_NAME, json.dumps(config).encode(), 0o600, meta["uid"], meta["gid"])
    if updated != raw:
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M%S-%f")
        backup = setup.with_name(setup.name + ".wb-cache-" + stamp + ".bak")
        with backup.open("xb") as f:
            f.write(raw)
        info = setup.stat()
        os.chmod(backup, stat.S_IMODE(info.st_mode))
        os.chown(backup, info.st_uid, info.st_gid)
        atomic_write(setup, updated, stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid)
        print("Backup: " + str(backup))
    print("Cache ready: both packages verified and gateway byte-range checks passed")
    print("Next wake prefers your gateway cache, with official routes retained as fallback")
    print("No gateway restart or GPU action performed. Keep the private cache JSON out of backups shared publicly.")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        # HTTP exceptions can contain private URLs. Keep their diagnostic surface narrow.
        message = str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__ + " " + str(getattr(exc, "code", ""))
        raise SystemExit("ERROR: " + message)
