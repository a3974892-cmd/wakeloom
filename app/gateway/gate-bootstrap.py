#!/usr/bin/env python3
"""Patch the next Colab boot; never restart the gate or allocate/stop a GPU.

Run beside this file: python3 gate-bootstrap.py
The default discovers the bind-mounted setup.py of the existing comfy-gate container.
Optional: python3 gate-bootstrap.py /actual/comfy-colab-kit
"""
import argparse
import ast
import datetime
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile

HELPERS = '\n# ── wb-bootstrap: named, bounded bootstrap jobs with captured child-process output ──\ndef _wb_bootstrap_tail(logfile):\n    with open(logfile, "rb") as f:\n        f.seek(0, 2)\n        f.seek(max(0, f.tell() - 8192))\n        text = f.read().decode("utf-8", "replace")\n    for key in ("HF_TOKEN", "CF_TUNNEL_TOKEN"):\n        value = os.environ.get(key)\n        if value:\n            text = text.replace(value, "[redacted]")\n    text = re.sub(r"(?i)(Bearer\\s+)\\S+", r"\\1[redacted]", text)\n    text = re.sub(r"(https?://)[^\\s/@]+:[^\\s/@]+@", r"\\1[redacted]@", text)\n    return "\\n".join(text.splitlines()[-20:])\n\n\ndef _wb_bootstrap_command(label, argv, cancel, attempts=1, timeout=300):\n    os.makedirs(LOG, exist_ok=True)\n    last = ""\n    for attempt in range(1, attempts + 1):\n        if cancel.is_set():\n            raise RuntimeError(f"{label}: canceled after another bootstrap failure")\n        logfile = os.path.join(LOG, f"bootstrap-{label}-{attempt}.log")\n        print(f"KIT_BOOTSTRAP {label} attempt {attempt}/{attempts}", flush=True)\n        started, next_note = time.monotonic(), time.monotonic() + 10\n        with open(logfile, "wb") as output:\n            p = subprocess.Popen(argv, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)\n            reason = ""\n            while p.poll() is None:\n                elapsed = time.monotonic() - started\n                if cancel.is_set() or elapsed >= timeout:\n                    reason = "canceled" if cancel.is_set() else f"timeout after {timeout}s"\n                    try:\n                        if os.name == "posix":\n                            os.killpg(p.pid, signal.SIGKILL)\n                        else:\n                            p.kill()\n                    except ProcessLookupError:\n                        pass\n                    p.wait()\n                    break\n                try:\n                    p.wait(timeout=min(1, max(0.01, timeout - elapsed)))\n                except subprocess.TimeoutExpired:\n                    pass\n                if time.monotonic() >= next_note:\n                    print(f"KIT_BOOTSTRAP {label} waiting {int(elapsed)}s", flush=True)\n                    next_note = time.monotonic() + 10\n        if p.returncode == 0 and not reason:\n            print(f"KIT_BOOTSTRAP {label} done", flush=True)\n            return\n        tail = _wb_bootstrap_tail(logfile)\n        last = f"{label}: {reason or \'exit \' + str(p.returncode)}"\n        print(f"KIT_BOOTSTRAP {last}; log={logfile}", flush=True)\n        for line in tail.splitlines():\n            print(f"  [{label}] {line}", flush=True)\n        if cancel.is_set():\n            raise RuntimeError(last)\n        if attempt < attempts:\n            print(f"KIT_BOOTSTRAP {label} retrying", flush=True)\n            cancel.wait(3)\n    detail = tail.splitlines()[-1] if tail else "no child-process output"\n    raise RuntimeError(f"bootstrap {last}: {detail}"[:700])\n\n\ndef _wb_bootstrap_download(urls, package, cancel, expected_size, expected_sha):\n    import hashlib\n    import urllib.request\n    import urllib.error\n    deadline = time.monotonic() + 180\n    last = "no download route"\n    for attempt in range(1, 3):\n        for route, url in enumerate(urls, 1):\n            if cancel.is_set():\n                raise RuntimeError("cloudflared-download: canceled")\n            if time.monotonic() >= deadline:\n                raise RuntimeError(f"cloudflared-download: total timeout; {last}")\n            print(f"KIT_BOOTSTRAP cloudflared-download route {route} attempt {attempt}/2", flush=True)\n            verified = False\n            try:\n                req = urllib.request.Request(url, headers={"User-Agent": "wakeloom-kit",\n                    "Accept": "application/octet-stream", "Cache-Control": "no-cache"})\n                digest, size = hashlib.sha256(), 0\n                route_deadline = min(deadline, time.monotonic() + 90)\n                next_note = time.monotonic() + 5\n                with urllib.request.urlopen(req, timeout=15) as response, open(package, "wb") as output:\n                    if response.status != 200:\n                        raise RuntimeError(f"unexpected HTTP {response.status}")\n                    while True:\n                        if cancel.is_set():\n                            raise RuntimeError("canceled")\n                        if time.monotonic() >= route_deadline:\n                            raise RuntimeError("download timeout")\n                        chunk = response.read(1024 * 1024)\n                        if not chunk:\n                            break\n                        size += len(chunk)\n                        if size > expected_size:\n                            raise RuntimeError("package exceeds expected size")\n                        output.write(chunk)\n                        digest.update(chunk)\n                        if time.monotonic() >= next_note:\n                            print(f"KIT_BOOTSTRAP cloudflared-download {size}/{expected_size} bytes", flush=True)\n                            next_note = time.monotonic() + 5\n                if size != expected_size or digest.hexdigest() != expected_sha:\n                    raise RuntimeError("package size or SHA-256 mismatch")\n                verified = True\n                print("KIT_BOOTSTRAP cloudflared-download verified", flush=True)\n                return\n            except Exception as exc:\n                # URLs may redirect to signed assets; never include them in public logs.\n                if isinstance(exc, urllib.error.HTTPError):\n                    last = f"HTTP {exc.code}"\n                elif isinstance(exc, RuntimeError):\n                    last = str(exc)\n                else:\n                    last = type(exc).__name__\n                print(f"KIT_BOOTSTRAP cloudflared-download route {route}: {last}", flush=True)\n                if cancel.is_set():\n                    raise RuntimeError("cloudflared-download: canceled") from None\n            finally:\n                if not verified and os.path.exists(package):\n                    os.unlink(package)\n        if attempt < 2:\n            cancel.wait(2)\n    raise RuntimeError(f"cloudflared-download: official routes failed; {last}")\n\n\ndef _wb_bootstrap_cloudflared(cancel):\n    if shutil.which("cloudflared"):\n        print("KIT_BOOTSTRAP cloudflared already installed", flush=True)\n        return\n    import tempfile\n    with tempfile.TemporaryDirectory(prefix="wb-cloudflared-") as folder:\n        package = os.path.join(folder, "cloudflared.deb")\n        # Pinned official 2026.10.7 asset: API route avoids the failing browser-download path.\n        _wb_bootstrap_download([\n            "https://api.github.com/repos/cloudflare/cloudflared/releases/assets/692817258",\n            "https://github.com/cloudflare/cloudflared/releases/download/2026.10.7/cloudflared-linux-amd64.deb",\n        ], package, cancel, 19925068, "70b1a1e0f46033ffc8d90b406e554044ba6cbf2e4360295ef1163aed31e09393")\n        _wb_bootstrap_command("cloudflared-install", ["dpkg", "-i", package], cancel, timeout=180)\n    if not shutil.which("cloudflared"):\n        raise RuntimeError("bootstrap cloudflared-install: executable missing after dpkg")\n\n\ndef _wb_bootstrap_comfy(cfg, cancel):\n    if os.path.isfile(os.path.join(COMFY, "main.py")):\n        print("KIT_BOOTSTRAP ComfyUI already installed", flush=True)\n        return\n    import tempfile\n    # Clone beside ongoing downloads. Only owned temporary checkouts are removed on retries.\n    for attempt in range(1, 4):\n        with tempfile.TemporaryDirectory(prefix="wb-comfy-") as folder:\n            checkout = os.path.join(folder, "source")\n            try:\n                _wb_bootstrap_command(f"comfyui-clone-{attempt}", ["git", "clone", "-q", cfg["repo"], checkout], cancel)\n            except RuntimeError:\n                if attempt == 3 or cancel.is_set():\n                    raise\n                print("KIT_BOOTSTRAP comfyui-clone retrying with a clean temporary checkout", flush=True)\n                cancel.wait(3)\n                continue\n            _wb_bootstrap_command("comfyui-checkout", ["git", "-C", checkout, "checkout", "-q", cfg["commit"]], cancel)\n            os.makedirs(COMFY, exist_ok=True)\n            # Match the original merge operation: preserve downloads and existing model links.\n            _wb_bootstrap_command("comfyui-copy", ["cp", "-a", checkout + "/.", COMFY + "/"], cancel, timeout=180)\n            if not os.path.isfile(os.path.join(COMFY, "main.py")):\n                raise RuntimeError("bootstrap comfyui-copy: main.py missing after checkout merge")\n            return\n\n\ndef _wb_bootstrap_uv(cancel):\n    try:\n        _wb_bootstrap_command("uv-install", [sys.executable, "-m", "pip", "install", "-q", "-U", "uv"],\n                              cancel, attempts=2, timeout=180)\n    except Exception:\n        if cancel.is_set():\n            raise\n        # Preserve the existing deps stage\'s pip fallback when uv is unavailable.\n        print("KIT_BOOTSTRAP uv unavailable; dependencies will use the existing pip fallback", flush=True)\n\n\ndef _wb_bootstrap(cfg):\n    from concurrent.futures import ThreadPoolExecutor, as_completed\n    cancel = threading.Event()\n    with ThreadPoolExecutor(max_workers=3, thread_name_prefix="wb-bootstrap") as pool:\n        jobs = [pool.submit(_wb_bootstrap_cloudflared, cancel), pool.submit(_wb_bootstrap_comfy, cfg, cancel),\n                pool.submit(_wb_bootstrap_uv, cancel)]\n        try:\n            for job in as_completed(jobs):\n                job.result()\n        except Exception:\n            cancel.set()\n            raise\n# ── wb-bootstrap end ──\n\n\n'
MAIN = '    with Stage("bootstrap"):\n        _wb_bootstrap(cfg)\n        dl.sync_links()\n        start_tunnel()\n        sh("nvidia-smi --query-gpu=name,memory.total --format=csv,noheader", check=False)\n\n    with Stage("deps"):\n'
SUPPORTED = ['    with Stage("bootstrap"):\n        # uv is only needed by the deps stage -> install it in the background while cloudflared\n        # and the ComfyUI clone run in parallel.\n        pip_uv = subprocess.Popen([sys.executable, "-m", "pip", "install", "-q", "-U", "uv"],\n                                  stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT)\n        procs = []\n        if not shutil.which("cloudflared"):\n            procs.append(subprocess.Popen(\n                "wget -q -O /tmp/cf.deb https://github.com/cloudflare/cloudflared/releases/latest/download/"\n                "cloudflared-linux-amd64.deb && dpkg -i /tmp/cf.deb >/dev/null", shell=True))\n        if not os.path.isfile(f"{COMFY}/main.py"):\n            # wb-boot: a model too big for the RAM store downloads straight into ComfyUI/models and can create\n            # the folder before this clone runs -> clone beside it and merge, instead of skipping the clone\n            procs.append(subprocess.Popen(\n                f"rm -rf /content/_comfy_src && git clone -q {cfg[\'repo\']} /content/_comfy_src && "\n                f"cd /content/_comfy_src && git checkout -q {cfg[\'commit\']} && mkdir -p {COMFY} && "\n                f"cp -a /content/_comfy_src/. {COMFY}/ && rm -rf /content/_comfy_src", shell=True))\n        for p in procs:\n            if p.wait() != 0:\n                raise RuntimeError("bootstrap step failed")\n        dl.sync_links()\n        start_tunnel()  # cloudflared retries until :8188 answers, so start it early\n        sh("nvidia-smi --query-gpu=name,memory.total --format=csv,noheader", check=False)\n        pip_uv.wait()  # uv must be on PATH before the deps stage uses it\n', '    with Stage("bootstrap"):\n        # uv is only needed by the deps stage -> install it in the background while cloudflared\n        # and the ComfyUI clone run in parallel.\n        pip_uv = subprocess.Popen([sys.executable, "-m", "pip", "install", "-q", "-U", "uv"],\n                                  stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT)\n        procs = []\n        if not shutil.which("cloudflared"):\n            procs.append(subprocess.Popen(\n                "wget -q -O /tmp/cf.deb https://github.com/cloudflare/cloudflared/releases/latest/download/"\n                "cloudflared-linux-amd64.deb && dpkg -i /tmp/cf.deb >/dev/null", shell=True))\n        if not os.path.isdir(COMFY):\n            procs.append(subprocess.Popen(\n                f"git clone -q {cfg[\'repo\']} {COMFY} && cd {COMFY} && git checkout -q {cfg[\'commit\']}", shell=True))\n        for p in procs:\n            if p.wait() != 0:\n                raise RuntimeError("bootstrap step failed")\n        dl.sync_links()\n        start_tunnel()  # cloudflared retries until :8188 answers, so start it early\n        sh("nvidia-smi --query-gpu=name,memory.total --format=csv,noheader", check=False)\n        pip_uv.wait()  # uv must be on PATH before the deps stage uses it\n', '    with Stage("bootstrap"):\n        _wb_bootstrap(cfg)\n        dl.sync_links()\n        start_tunnel()\n        sh("nvidia-smi --query-gpu=name,memory.total --format=csv,noheader", check=False)\n\n']
START = "    with Stage(\"bootstrap\"):\n"
END = "\n    with Stage(\"deps\"):\n"
MARKER = "# ── wb-bootstrap:"
FINISH = "# ── wb-bootstrap end ──"


def patch_source(text):
    crlf = "\r\n" in text
    updated = _patch_source_lf(text.replace("\r\n", "\n"))
    return updated.replace("\n", "\r\n") if crlf else updated


def _patch_source_lf(text):
    tree = ast.parse(text)
    mains = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "main"]
    if len(mains) != 1:
        raise RuntimeError("Expected exactly one setup main(); nothing changed")
    if HELPERS.strip() in text and MAIN in text:
        return text
    lines = text.splitlines(keepends=True)
    begin = sum(map(len, lines[:mains[0].lineno - 1]))
    end = sum(map(len, lines[:mains[0].end_lineno]))
    main = text[begin:end]
    if main.count(START) != 1 or main.count(END) != 1:
        raise RuntimeError("Bootstrap boundaries differ from the supported kit; nothing changed")
    start = begin + main.index(START)
    stop = begin + main.index(END)
    current = text[start:stop]
    def signature(body):
        return ast.dump(ast.parse("def fixture():\n" + body).body[0], include_attributes=False)
    if signature(current) not in {signature(body) for body in SUPPORTED}:
        raise RuntimeError("Custom bootstrap detected; refusing to overwrite it")
    text = text[:start] + MAIN + text[stop + len(END):]
    if MARKER in text:
        if text.count(MARKER) != 1 or text.count(FINISH) != 1:
            raise RuntimeError("Ambiguous bootstrap helper markers; nothing changed")
        i, j = text.index(MARKER), text.index(FINISH) + len(FINISH)
        text = text[:i] + HELPERS.strip() + text[j:]
    else:
        position = text.index("def main():", begin)
        text = text[:position] + HELPERS.strip() + "\n\n\n" + text[position:]
    compile(text, "setup.py", "exec")
    return text


def discover(root=None, container="comfy-gate"):
    if root:
        root = Path(root).resolve()
        candidates = [root / "packages/colab/setup.py", root / "colab/setup.py"]
        if root.is_file():
            candidates.insert(0, root)
        for path in candidates:
            if path.is_file():
                return path.resolve()
        raise RuntimeError("No setup.py found in the specified gateway folder")
    result = subprocess.run(["docker", "inspect", "--format", "{{json .Mounts}}", container],
                            check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    for mount in json.loads(result.stdout):
        destination = mount.get("Destination", "").rstrip("/")
        if mount.get("Type") != "bind":
            continue
        if destination == "/app/packages/colab":
            path = Path(mount["Source"]) / "setup.py"
        elif destination == "/app/packages/colab/setup.py":
            path = Path(mount["Source"])
        else:
            continue
        if path.is_file():
            return path.resolve()
    raise RuntimeError("No supported host bind mount found; nothing changed. Do not restart the container")


def install(path, check=False):
    raw = path.read_bytes()
    text = raw.decode("utf-8")
    updated = patch_source(text)
    if updated == text:
        print("Already patched: " + str(path))
        return
    print("Validated bootstrap update: " + str(path))
    if check:
        print("Check only; nothing changed")
        return
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M%S-%f")
    backup = path.with_name(path.name + ".wb-bootstrap-" + stamp + ".bak")
    metadata = path.stat()
    with backup.open("xb") as f:
        f.write(raw)
    os.chmod(backup, stat.S_IMODE(metadata.st_mode))
    fd, temporary = tempfile.mkstemp(prefix=".wb-bootstrap-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as f:
            f.write(updated)
        os.chmod(temporary, stat.S_IMODE(metadata.st_mode))
        if hasattr(os, "geteuid") and os.geteuid() == 0:
            os.chown(temporary, metadata.st_uid, metadata.st_gid)
            os.chown(backup, metadata.st_uid, metadata.st_gid)
        if path.read_bytes() != raw:
            raise RuntimeError("Source changed during patching; refusing to replace it")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    print("Backup: " + str(backup))
    print("Done. Next GPU wake uses named bootstrap logs and bounded retries. No restart or GPU action performed.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", nargs="?")
    parser.add_argument("--container", default="comfy-gate")
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    install(discover(args.root, args.container), args.check)


if __name__ == "__main__":
    main()
