"""Real compressed archives and simulated HTTP failures; no GPU, model weights or network."""
import ast
import contextlib
import hashlib
import importlib.util
import io
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import time
import types
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

import zstandard
from ollama_installer import INSTALL_SOURCE

ROOT = Path(__file__).resolve().parents[2]


def archive(extra=None):
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode="w") as tar:
        for name, content in (("bin/ollama", b"test executable"), ("lib/ollama/library.so", b"test library")):
            member = tarfile.TarInfo(name)
            member.size, member.mode = len(content), 0o755
            tar.addfile(member, io.BytesIO(content))
        if extra:
            member = tarfile.TarInfo(extra)
            member.size = 4
            tar.addfile(member, io.BytesIO(b"evil"))
    return zstandard.ZstdCompressor().compress(data.getvalue())


def environment(root, raw):
    ns = dict(os=os, sys=sys, time=time, subprocess=subprocess, urllib=__import__("urllib"), LOG=str(root / "logs"))
    exec(compile(INSTALL_SOURCE, "<installer>", "exec"), ns)
    ns.update(OLLAMA_INSTALL_SIZE=len(raw), OLLAMA_INSTALL_SHA256=hashlib.sha256(raw).hexdigest(),
              OLLAMA_INSTALL_PREFIX=str(root / "prefix"))
    (root / "prefix").mkdir(exist_ok=True)
    return ns


class OllamaDownloadTests(unittest.TestCase):
    def test_504_uses_other_official_source_and_installs_verified_archive(self):
        raw = archive()
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()) as out:
            root = Path(temp)
            ns = environment(root, raw)
            calls = []

            def urlopen(request, **kwargs):
                calls.append(request.full_url)
                if len(calls) == 1:
                    raise urllib.error.HTTPError(request.full_url, 504, "Gateway Time-out", None, None)
                return io.BytesIO(raw)

            checked = types.SimpleNamespace(returncode=0, stdout=b"ollama version is 0.40.15")
            with patch.object(urllib.request, "urlopen", side_effect=urlopen), patch.object(subprocess, "run", return_value=checked) as run:
                ns["ollama_install"]()
            self.assertEqual(calls, list(ns["OLLAMA_INSTALL_URLS"])[:2])
            self.assertIn("HTTP 504", out.getvalue())
            self.assertIn("SHA-256 verified", out.getvalue())
            self.assertEqual((root / "prefix/bin/ollama").read_bytes(), b"test executable")
            self.assertEqual((root / "prefix/lib/ollama/library.so").read_bytes(), b"test library")
            self.assertEqual(run.call_args.args[0][-1], "--version")
            self.assertFalse(any((root / "prefix").glob(".wb-ollama-*")))

    def test_all_sources_fail_without_modifying_existing_installation(self):
        raw = archive()
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            ns = environment(root, raw)
            old = root / "prefix/bin/ollama"
            old.parent.mkdir()
            old.write_bytes(b"old model runtime")
            with patch.object(urllib.request, "urlopen", side_effect=urllib.error.HTTPError("https://official.invalid", 504, "timeout", None, None)) as request, patch.object(time, "sleep"):
                with self.assertRaisesRegex(RuntimeError, "HTTP 504"):
                    ns["ollama_install"]()
            self.assertEqual(request.call_count, 6)
            self.assertEqual(old.read_bytes(), b"old model runtime")

    def test_corrupt_archive_uses_fallback_and_path_escape_is_rejected(self):
        raw = archive()
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            ns = environment(root, raw)
            with patch.object(urllib.request, "urlopen", side_effect=[io.BytesIO(b"corrupt"), io.BytesIO(raw)]):
                ns["_ollama_install_archive"](str(root / "download"), lambda _: None)
            self.assertEqual((root / "download").read_bytes(), raw)
            (root / "unsafe.zst").write_bytes(archive("../escape"))
            (root / "stage").mkdir()
            with self.assertRaisesRegex(RuntimeError, "unsafe path"):
                ns["_ollama_install_extract"](str(root / "unsafe.zst"), str(root / "stage"), lambda _: None)
            self.assertFalse((root / "escape").exists())

    def test_publish_failure_restores_old_binary_and_libraries(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            ns = environment(root, archive())
            for base, word in ((root / "prefix", b"old"), (root / "stage", b"new")):
                (base / "bin").mkdir(parents=True, exist_ok=True)
                (base / "lib/ollama").mkdir(parents=True)
                (base / "bin/ollama").write_bytes(word)
                (base / "lib/ollama/library.so").write_bytes(word)
            replace = os.replace
            failed = False

            def fail_second(source, target):
                nonlocal failed
                if not failed and Path(source) == root / "stage/bin/ollama":
                    failed = True
                    raise OSError("simulated filesystem failure")
                return replace(source, target)

            checked = types.SimpleNamespace(returncode=0, stdout=b"ollama version is 0.40.15")
            with patch.object(subprocess, "run", return_value=checked), patch.object(os, "replace", side_effect=fail_second):
                with self.assertRaisesRegex(OSError, "filesystem"):
                    ns["_ollama_install_publish"](str(root / "stage"), str(root / "prefix"), lambda _: None)
            self.assertEqual((root / "prefix/bin/ollama").read_bytes(), b"old")
            self.assertEqual((root / "prefix/lib/ollama/library.so").read_bytes(), b"old")

    def test_narrow_patch_preserves_bootstrap_profiles_and_other_functions(self):
        spec = importlib.util.spec_from_file_location("ollama_patch", ROOT / "gateway/gate-ollama-download.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        original = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
        if module.MARKER in original:
            start = original.index(module.MARKER)
            stop = original.index(module.FINISH, start) + len(module.FINISH)
            original = original[:start] + module.LEGACY + original[stop:]
        updated = module.patch_source(original)
        self.assertEqual(module.patch_source(updated), updated)
        before = ast.parse(original)
        after = ast.parse(updated)
        for name in ("main", "ai_finish", "ai_prepare", "ollama_serve"):
            first = next(n for n in before.body if isinstance(n, ast.FunctionDef) and n.name == name)
            last = next(n for n in after.body if isinstance(n, ast.FunctionDef) and n.name == name)
            self.assertEqual(ast.dump(first), ast.dump(last), name)
        with self.assertRaisesRegex(RuntimeError, "Custom Ollama installer"):
            module.patch_source(original.replace('last = None\n    for attempt in range(1, 4):', 'last = "user customization"\n    for attempt in range(1, 4):'))
        current = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
        # The desktop kit now enables the shared bootstrap fix. This narrow Ollama
        # patch must also leave those helpers and the entry point unchanged.
        self.assertIn("def _wb_bootstrap(", current)
        self.assertEqual(module.patch_source(current), current)


if __name__ == "__main__":
    unittest.main()
