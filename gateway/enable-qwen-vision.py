"""Run inside an existing Colab VM. Add the matching projector; never stop the VM.

Wakeloom local mode uploads this file through its authenticated Jupyter connection.
The VPS helper runs the identical file through the gateway's existing Colab CLI.
Only the built-in, pinned Qwen3.8 27B Q8 model is supported by this installer.
"""
import contextlib
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import struct
import tempfile
import threading
import time
import urllib.request
import zlib

REV = "1c69098e9c32db6dc06fc48f1d6cc75ad07da89e"
MAIN_SHA = "427b9416c2aba9d21da4e90ddac50a2bc8b6d0bfef675af3580a43aa38e000c3"
MAIN_BYTES = 29047084320
PROJECTOR_SHA = "c9a09064683620bea3d3bfed5d4462e1a97a7d2fff7e5045d6862a0a85eeb5b5"
PROJECTOR_BYTES = 931145888
PROJECTOR_URL = f"https://huggingface.co/huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF/resolve/{REV}/mmproj-model-bf16.gguf"
VISION_NAME = "qwen3.8-27b-vision"


def progress(phase, text):
    print("WB_VISION " + json.dumps({"phase": phase, "text": text}), flush=True)


def ollama(path, body=None, timeout=60):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request("http://127.0.0.1:11434/api/" + path, data=data,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as response:
        result = json.load(response)
    if result.get("error"):
        raise RuntimeError(str(result["error"]))
    return result


def vision_probe_image():
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff)
    png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 32, 32, 8, 2, 0, 0, 0))
    png += chunk(b"IDAT", zlib.compress((b"\0" + bytes([255, 0, 0]) * 32) * 32)) + chunk(b"IEND", b"")
    return base64.b64encode(png).decode("ascii")


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(8 << 20), b""):
            h.update(block)
    return h.hexdigest()


def download_projector(dest):
    if dest.is_file() and dest.stat().st_size == PROJECTOR_BYTES and digest(dest) == PROJECTOR_SHA:
        progress("download", "Using the verified projector already on this VM")
        return
    dest.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=".wb-mmproj-", suffix=".part", dir=dest.parent)
    try:
        h = hashlib.sha256()
        done, last = 0, time.monotonic()
        progress("download", "Downloading the 0.94 GB vision projector")
        with os.fdopen(fd, "wb") as out, urllib.request.urlopen(PROJECTOR_URL, timeout=60) as response:
            while True:
                block = response.read(8 << 20)
                if not block:
                    break
                done += len(block)
                if done > PROJECTOR_BYTES:
                    raise RuntimeError("Projector download exceeds its pinned size")
                h.update(block)
                out.write(block)
                if time.monotonic() - last >= 5:
                    progress("download", f"Projector: {done * 100 // PROJECTOR_BYTES}%")
                    last = time.monotonic()
        if done != PROJECTOR_BYTES or h.hexdigest() != PROJECTOR_SHA:
            raise RuntimeError("Projector checksum/size mismatch; the original model is unchanged")
        os.replace(name, dest)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def create_model(source, projector, models):
    binary = shutil.which("ollama") or "/usr/local/bin/ollama"
    if not Path(binary).is_file():
        raise RuntimeError("Ollama is not installed")
    # Reuse Ollama's existing main blob. Only the small projector may need copying.
    blob = models / "blobs" / ("sha256-" + PROJECTOR_SHA)
    if not blob.exists():
        try:
            os.link(projector, blob)
        except OSError:
            pass  # Standard Ollama create remains the fallback.
    env = {**os.environ, "OLLAMA_HOST": "127.0.0.1:11434", "OLLAMA_MODELS": str(models)}
    for secret in ("HF_TOKEN", "CF_TUNNEL_TOKEN"):
        env.pop(secret, None)
    with tempfile.TemporaryDirectory(prefix="wb-vision-") as temp:
        mf = Path(temp) / "Modelfile"
        mf.write_text(f'FROM "{source}"\nFROM "{projector}"\n', encoding="utf-8")
        progress("register", "Registering vision using the existing main model")
        result = subprocess.run([binary, "create", VISION_NAME, "-f", str(mf)], env=env,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=900)
        if result.returncode:
            raise RuntimeError("Vision registration failed: " + result.stdout.decode("utf-8", "replace")[-800:])


def enable_vision(info_path=Path("/content/wb_ai.json"), models=Path("/content/ollama_models"),
                  api=ollama, download=download_projector, create=create_model):
    info = json.loads(info_path.read_text(encoding="utf-8"))
    llm = info.get("llm") or {}
    current = llm.get("name")
    if info.get("type") != "llm" or current not in ("qwen3.8-27b", VISION_NAME):
        raise RuntimeError("This installer requires the built-in Qwen3.8 27B model")
    if "vision" in (api("show", {"model": current}).get("capabilities") or []):
        progress("ready", "Vision is already enabled")
        return
    source = models / "blobs" / ("sha256-" + MAIN_SHA)
    if not source.is_file() or source.stat().st_size != MAIN_BYTES:
        raise RuntimeError("The installed model does not match the supported Q8_0 weights; choose the vision startup profile")
    projector = models / "wb-vision" / "mmproj-model-bf16.gguf"
    download(projector)
    create(source, projector, models)
    if "vision" not in (api("show", {"model": VISION_NAME}).get("capabilities") or []):
        raise RuntimeError("Ollama did not enable vision. The original model remains selected; update Ollama before retrying")
    progress("load", "Loading the vision model; the GPU session stays running")
    try:
        api("generate", {"model": VISION_NAME, "keep_alive": -1, "stream": False,
                         "options": {"num_ctx": int(llm.get("ctx", 32768))}}, timeout=1200)
        progress("test", "Checking real image inference with a synthetic test image")
        answer = api("chat", {"model": VISION_NAME, "stream": False, "think": False, "keep_alive": -1,
                              "messages": [{"role": "user", "content": "What is the dominant color in this image? Reply briefly.",
                                            "images": [vision_probe_image()]}],
                              "options": {"num_ctx": int(llm.get("ctx", 32768)), "num_predict": 32, "temperature": 0}}, timeout=1200)
        if answer.get("error") or not (answer.get("message") or {}).get("content", "").strip():
            raise RuntimeError("Vision image inference did not return an answer; the original model remains selected")
    except Exception:
        # Restore the original loaded model if Ollama's scheduler evicted it while trying vision.
        with contextlib.suppress(Exception):
            api("generate", {"model": current, "keep_alive": -1, "stream": False}, timeout=1200)
        raise
    updated = json.loads(json.dumps(info))
    updated["llm"].update(name=VISION_NAME, projector="qwen38-vision-projector", projector_dest=str(projector))
    updated["ready"] = True
    fd, name = tempfile.mkstemp(prefix=".wb-ai-", suffix=".json", dir=info_path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(updated, f)
        os.replace(name, info_path)
    finally:
        if os.path.exists(name):
            os.unlink(name)
    progress("ready", "Vision enabled; existing conversations and the text model are preserved")


def main():
    stop = threading.Event()

    def heartbeat():
        while not stop.is_set():
            with contextlib.suppress(OSError):
                Path("/content/kit_heartbeat").touch()
            stop.wait(15)

    thread = threading.Thread(target=heartbeat, daemon=True)
    thread.start()
    try:
        enable_vision()
    finally:
        stop.set()
        thread.join(1)


if __name__ == "__main__":
    main()
