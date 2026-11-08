#!/usr/bin/env python3
"""Fix Ollama HTTP 504 downloads for the next Colab startup.

python3 gate-ollama-download.py --container comfy-gate
Only the recognized Ollama installer is changed. No bootstrap/profile edits, gate restart or GPU action.
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

SOURCE = '\n# ── wb-ollama-download: verified release, official fallback and visible progress ──\nOLLAMA_INSTALL_VERSION = "0.40.15"\nOLLAMA_INSTALL_PREFIX = "/usr/local"\nOLLAMA_INSTALL_SIZE = 1543547172\nOLLAMA_INSTALL_SHA256 = "628c018404a4206493efd38e46251c154ede5141b8b8dc85c659b3b027c47379"\nOLLAMA_INSTALL_URLS = (\n    "https://api.github.com/repos/ollama/ollama/releases/assets/715495290",\n    "https://github.com/ollama/ollama/releases/download/v0.40.15/ollama-linux-amd64.tar.zst",\n    "https://ollama.com/download/ollama-linux-amd64.tar.zst?version=0.40.15",\n)\n\n\ndef _ollama_install_archive(dest, note):\n    import hashlib\n    import urllib.parse\n    errors = []\n    for round_no in range(1, 3):\n        for url in OLLAMA_INSTALL_URLS:\n            host = urllib.parse.urlsplit(url).hostname\n            note(f"download {host}, attempt {round_no}/2, version {OLLAMA_INSTALL_VERSION}")\n            try:\n                request = urllib.request.Request(url, headers={"User-Agent": "wakeloom-kit", "Accept": "application/octet-stream", "Cache-Control": "no-cache"})\n                with urllib.request.urlopen(request, timeout=30) as response, open(dest, "wb") as out:\n                    digest = hashlib.sha256()\n                    done, started, last = 0, time.monotonic(), time.monotonic()\n                    while True:\n                        block = response.read(8 << 20)\n                        if not block:\n                            break\n                        done += len(block)\n                        if done > OLLAMA_INSTALL_SIZE:\n                            raise RuntimeError("release exceeds its pinned size")\n                        out.write(block)\n                        digest.update(block)\n                        if time.monotonic() - last >= 5:\n                            note(f"downloaded {done / 1e6:.0f}/{OLLAMA_INSTALL_SIZE / 1e6:.0f} MB")\n                            last = time.monotonic()\n                        if time.monotonic() - started > 360:\n                            raise TimeoutError("release download exceeded 360 seconds")\n                if done != OLLAMA_INSTALL_SIZE or digest.hexdigest() != OLLAMA_INSTALL_SHA256:\n                    raise RuntimeError("release size/SHA-256 mismatch; nothing installed")\n                note("release size and SHA-256 verified")\n                return\n            except Exception as error:\n                code = getattr(error, "code", None)\n                detail = f"HTTP {code}" if code else type(error).__name__ + ": " + str(error)[:180]\n                errors.append(f"{host}: {detail}")\n                note(f"{host} failed: {detail}; trying the next official source")\n        if round_no < 2:\n            time.sleep(3)\n    raise RuntimeError("Ollama official downloads failed: " + "; ".join(errors[-4:]))\n\n\ndef _ollama_install_extract(archive, stage, note):\n    import tarfile\n    import pathlib\n    import zstandard\n    root = os.path.realpath(stage)\n    last = time.monotonic()\n    with open(archive, "rb") as source, zstandard.ZstdDecompressor().stream_reader(source) as reader, \\\n            tarfile.open(fileobj=reader, mode="r|") as tar:\n        for member in tar:\n            name = pathlib.PurePosixPath(member.name)\n            if name.is_absolute() or ".." in name.parts or "\\\\" in member.name or ":" in member.name:\n                raise RuntimeError("unsafe path in Ollama release")\n            target = os.path.realpath(os.path.join(root, *name.parts))\n            if os.path.commonpath([root, target]) != root or member.isdev() or member.isfifo():\n                raise RuntimeError("unsafe entry in Ollama release")\n            if member.issym() or member.islnk():\n                link = pathlib.PurePosixPath(member.linkname)\n                base = os.path.dirname(target) if member.issym() else root\n                resolved = os.path.realpath(os.path.join(base, member.linkname))\n                if link.is_absolute() or os.path.commonpath([root, resolved]) != root:\n                    raise RuntimeError("unsafe link in Ollama release")\n            try:\n                tar.extract(member, stage, filter="data")\n            except TypeError:  # Older Python; path/type/link checks above still apply.\n                tar.extract(member, stage)\n            if time.monotonic() - last >= 5:\n                note("extracting the verified Ollama release")\n                last = time.monotonic()\n    binary = os.path.join(stage, "bin", "ollama")\n    if not os.path.isfile(binary) or os.path.getsize(binary) == 0:\n        raise RuntimeError("bin/ollama missing or empty in verified release")\n    if not os.path.isdir(os.path.join(stage, "lib", "ollama")):\n        raise RuntimeError("lib/ollama missing in verified release")\n    os.chmod(binary, 0o755)\n\n\ndef _ollama_install_publish(stage, prefix, note):\n    import tempfile\n    env = {k: v for k, v in os.environ.items() if k not in ("HF_TOKEN", "CF_TUNNEL_TOKEN")}\n    binary = os.path.join(stage, "bin", "ollama")\n    checked = subprocess.run([binary, "--version"], env=env, stdout=subprocess.PIPE,\n                             stderr=subprocess.STDOUT, timeout=30)\n    version = checked.stdout.decode("utf-8", "replace")\n    if checked.returncode or OLLAMA_INSTALL_VERSION not in version:\n        raise RuntimeError("downloaded Ollama executable failed its version check")\n    # Stage and backup are on the installation filesystem. Restore old paths if publishing fails.\n    with tempfile.TemporaryDirectory(prefix=".wb-ollama-previous-", dir=prefix) as previous:\n        changes = []\n        try:\n            for relative in ("lib/ollama", "bin/ollama"):\n                target = os.path.join(prefix, relative)\n                old = os.path.join(previous, relative)\n                os.makedirs(os.path.dirname(target), exist_ok=True)\n                os.makedirs(os.path.dirname(old), exist_ok=True)\n                existed = os.path.lexists(target)\n                if existed:\n                    os.replace(target, old)\n                changes.append((relative, existed))\n                os.replace(os.path.join(stage, relative), target)\n        except Exception:\n            for relative, existed in reversed(changes):\n                target, old = os.path.join(prefix, relative), os.path.join(previous, relative)\n                if os.path.lexists(target):\n                    os.replace(target, os.path.join(stage, relative))\n                if existed:\n                    os.replace(old, target)\n            raise\n    note("installed and checked Ollama " + OLLAMA_INSTALL_VERSION)\n\n\ndef ollama_install():\n    """Use official asset/download routes for one pinned release; no install.sh, apt or systemd."""\n    import tempfile\n    os.makedirs(LOG, exist_ok=True)\n    with open(os.path.join(LOG, "ollama_install.log"), "a", encoding="utf-8") as log:\n        def note(message):\n            message = "KIT_OLLAMA_INSTALL " + message\n            log.write(message + "\\n")\n            log.flush()\n            print(message, flush=True)\n\n        try:\n            import zstandard\n        except ImportError:\n            for attempt in (1, 2):\n                note(f"installing archive decoder, attempt {attempt}/2")\n                try:\n                    result = subprocess.run([sys.executable, "-m", "pip", "install", "-q", "zstandard"],\n                                            stdout=log, stderr=subprocess.STDOUT, timeout=90)\n                    if result.returncode:\n                        raise RuntimeError("archive decoder installer failed")\n                    import zstandard\n                    break\n                except Exception:\n                    if attempt == 2:\n                        raise RuntimeError("Unable to install the zstandard archive decoder") from None\n        prefix = OLLAMA_INSTALL_PREFIX\n        with tempfile.TemporaryDirectory(prefix=".wb-ollama-install-", dir=prefix) as temporary:\n            archive = os.path.join(temporary, "release.tar.zst")\n            stage = os.path.join(temporary, "stage")\n            os.makedirs(stage)\n            _ollama_install_archive(archive, note)\n            _ollama_install_extract(archive, stage, note)\n            _ollama_install_publish(stage, prefix, note)\n# ── wb-ollama-download end ──\n'
LEGACY = 'def ollama_install():\n    """Install the Ollama release tarball into /usr/local without install.sh: no apt / dpkg (whose lock the\n    concurrent cloudflared .deb install holds), no systemd. The .tar.zst is streamed through Python."""\n    import tarfile\n    logf = open(f"{LOG}/ollama_install.log", "a")\n\n    def note(msg):\n        logf.write(msg + "\\n")\n        logf.flush()\n\n    try:\n        import zstandard\n    except ImportError:\n        subprocess.run([sys.executable, "-m", "pip", "install", "-q", "zstandard"], stdout=logf, stderr=subprocess.STDOUT)\n        import zstandard\n    last = None\n    for attempt in range(1, 4):\n        try:\n            req = urllib.request.Request(OLLAMA_URL_DL, headers={"User-Agent": "wakeloom-kit"})\n            with urllib.request.urlopen(req, timeout=60) as r:\n                note(f"download {r.geturl()[:120]} ({r.headers.get(\'Content-Length\')} bytes)")\n                with zstandard.ZstdDecompressor().stream_reader(r) as zr, tarfile.open(fileobj=zr, mode="r|") as tf:\n                    try:\n                        tf.extractall("/usr/local", filter="tar")\n                    except TypeError:\n                        tf.extractall("/usr/local")\n            if os.path.isfile("/usr/local/bin/ollama"):\n                os.chmod("/usr/local/bin/ollama", 0o755)\n                note("installed /usr/local/bin/ollama")\n                return\n            last = RuntimeError("bin/ollama missing from the archive")\n        except Exception as e:\n            last = e\n        note(f"attempt {attempt} failed: {last!r}")\n        time.sleep(5 * attempt)\n    raise RuntimeError(f"Ollama install failed: {last!r}")\n'
MARKER = "# ── wb-ollama-download:"
FINISH = "# ── wb-ollama-download end ──"


def patch_source(text):
    crlf = "\r\n" in text
    updated = patch_source_lf(text.replace("\r\n", "\n"))
    return updated.replace("\n", "\r\n") if crlf else updated


def patch_source_lf(text):
    tree = ast.parse(text)
    installers = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "ollama_install"]
    if len(installers) != 1:
        raise RuntimeError("Expected one known Ollama installer; nothing changed")
    if SOURCE.strip() in text:
        return text
    if MARKER in text or FINISH in text:
        raise RuntimeError("Different Ollama download patch detected; preserve it for review instead of overwriting")
    original = installers[0]
    if ast.dump(original, include_attributes=False) != ast.dump(ast.parse(LEGACY).body[0], include_attributes=False):
        raise RuntimeError("Custom Ollama installer detected; refusing to overwrite it")
    lines = text.splitlines(keepends=True)
    first = sum(map(len, lines[:original.lineno - 1]))
    last = sum(map(len, lines[:original.end_lineno]))
    updated = text[:first] + SOURCE.strip() + "\n" + text[last:]
    compile(updated, "setup.py", "exec")
    return updated

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
    print("Validated Ollama download update: " + str(path))
    if check:
        print("Check only; nothing changed")
        return
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M%S-%f")
    backup = path.with_name(path.name + ".wb-ollama-download-" + stamp + ".bak")
    metadata = path.stat()
    with backup.open("xb") as f:
        f.write(raw)
    os.chmod(backup, stat.S_IMODE(metadata.st_mode))
    fd, temporary = tempfile.mkstemp(prefix=".wb-ollama-download-", dir=path.parent)
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
    print("Done. Next GPU wake uses verified Ollama official downloads with fallback. No restart or GPU action performed.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", nargs="?")
    parser.add_argument("--container", default="comfy-gate")
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    install(discover(args.root, args.container), args.check)


if __name__ == "__main__":
    main()
