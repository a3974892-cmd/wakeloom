"""Build a bootstrap-only VPS patch from the same source as gate-ai.py and the embedded kit."""

TEMPLATE = r'''#!/usr/bin/env python3
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

HELPERS = __WB_HELPERS__
MAIN = __WB_MAIN__
SUPPORTED = __WB_SUPPORTED__
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
'''


def generate(helpers, main, supported):
    assert helpers.endswith("def main():")
    helpers = helpers[:-len("def main():")]
    return TEMPLATE.replace("__WB_HELPERS__", repr(helpers)).replace("__WB_MAIN__", repr(main)).replace("__WB_SUPPORTED__", repr(supported))
