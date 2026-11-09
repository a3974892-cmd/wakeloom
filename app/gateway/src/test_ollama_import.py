"""Exercise real hard links and the unchanged CLI path without CUDA, model downloads or a VM."""
import ast
import contextlib
import errno
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]


def require_symlink(directory):
    source = directory / "symlink-probe-source"
    link = directory / "symlink-probe-link"
    source.write_bytes(b"probe")
    try:
        os.symlink(str(source), str(link))
    except OSError as error:
        if getattr(error, "winerror", None) == 1314:
            raise unittest.SkipTest("Windows token cannot create symlinks; Linux/native-link test required")
        raise
    finally:
        if os.path.lexists(link):
            link.unlink()
        source.unlink()


def load_functions(models, popen=None):
    source = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
    names = {"_ollama_seed_blob", "_ollama_create", "_dir_bytes", "_ollama_check_import_space"}
    functions = [n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name in names]
    if len(functions) != len(names):
        raise AssertionError("generated kit missing importer functions")
    env = dict(os=os, time=time, re=re, threading=threading, shutil=shutil,
               subprocess=types.SimpleNamespace(Popen=popen or subprocess.Popen, TimeoutExpired=subprocess.TimeoutExpired,
                                                PIPE=subprocess.PIPE, STDOUT=subprocess.STDOUT),
               _ollama_env=lambda: {**os.environ, "OLLAMA_MODELS": str(models)}, _ollama_bin=lambda: "test-ollama")
    exec(compile(ast.Module(body=functions, type_ignores=[]), "<kit ollama import>", "exec"), env)
    return env


class OllamaImportTests(unittest.TestCase):
    def test_real_link_preserves_model_and_reuses_bytes(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            model = root / "model.gguf"
            original = b"GGUF" + bytes(range(256)) * 100
            model.write_bytes(original)
            env = load_functions(root / "ollama")
            self.assertTrue(env["_ollama_seed_blob"](str(model)))
            blob = root / "ollama/blobs" / ("sha256-" + hashlib.sha256(original).hexdigest())
            self.assertTrue(os.path.samefile(model, blob))
            self.assertEqual(model.read_bytes(), original)
            self.assertEqual(blob.stat().st_size, len(original))
            # Repeated registration must not replace any existing blob.
            self.assertFalse(env["_ollama_seed_blob"](str(model)))
            self.assertTrue(os.path.samefile(model, blob))

    def test_cli_registration_and_copy_fallback(self):
        for fallback in (False, True, "cross_fs"):
            with self.subTest(fallback=fallback), tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
                root = Path(temp)
                if fallback == "cross_fs":
                    require_symlink(root)
                source = root / "model.gguf"
                source.write_bytes(b"GGUF unchanged bytes" * 100)
                original = source.read_bytes()
                mf = root / "Modelfile"
                mf.write_text(f"FROM {source}\n", encoding="utf-8")
                record = root / "record.json"
                cli = root / "fake_cli.py"
                cli.write_text('''import hashlib, json, os, pathlib, shutil, sys
assert sys.argv[1:3] == ["create", "test-model"]
assert sys.argv[3] == "-f"
source = pathlib.Path(pathlib.Path(sys.argv[4]).read_text(encoding="utf-8").strip()[5:])
digest = hashlib.sha256(source.read_bytes()).hexdigest()
blob = pathlib.Path(os.environ["OLLAMA_MODELS"]) / "blobs" / ("sha256-" + digest)
blob.parent.mkdir(parents=True, exist_ok=True)
copied = not blob.exists()
if copied: shutil.copyfile(source, blob)
assert blob.read_bytes() == source.read_bytes()
pathlib.Path(sys.argv[4]).with_name("record.json").write_text(json.dumps({"copied": copied, "linked": os.path.samefile(source, blob)}))
print("using existing layer sha256:" + digest, flush=True)
print("success", flush=True)
''', encoding="utf-8")

                def popen(argv, **kwargs):
                    return subprocess.Popen([sys.executable, str(cli), *argv[1:]], **kwargs)

                env = load_functions(root / "ollama", popen)
                link_error = errno.EXDEV if fallback == "cross_fs" else errno.EPERM
                with patch.object(os, "link", side_effect=OSError(link_error, "hard links unavailable")) if fallback else contextlib.nullcontext():
                    env["_ollama_create"]("test-model", str(mf), str(source), every=1)
                result = json.loads(record.read_text())
                self.assertEqual(result["copied"], fallback is True)
                self.assertEqual(result["linked"], fallback is not True)
                blob = root / "ollama/blobs" / ("sha256-" + hashlib.sha256(original).hexdigest())
                self.assertEqual(blob.is_symlink(), fallback == "cross_fs")
                self.assertEqual(source.read_bytes(), original)
                self.assertEqual(mf.read_text(), f"FROM {source}\n")

    def test_cross_filesystem_verifies_symlink_and_import_failure_still_fails(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            require_symlink(root)
            source = root / "model.gguf"
            source.write_bytes(b"GGUF test")
            models = root / "ollama"
            (models / "blobs").mkdir(parents=True)
            env = load_functions(models)
            real_stat = os.stat

            def stat(p, *args, **kwargs):
                if str(p) == str(models / "blobs"):
                    return types.SimpleNamespace(st_dev=real_stat(source).st_dev + 1)
                return real_stat(p, *args, **kwargs)

            env["os"] = types.SimpleNamespace(path=os.path, makedirs=os.makedirs, stat=stat,
                fstat=os.fstat, symlink=os.symlink, unlink=os.unlink,
                link=lambda *a: self.fail("cross-filesystem path must not attempt hard links"))
            self.assertTrue(env["_ollama_seed_blob"](str(source)))
            blob = models / "blobs" / ("sha256-" + hashlib.sha256(source.read_bytes()).hexdigest())
            self.assertTrue(blob.is_symlink())
            self.assertTrue(os.path.samefile(source, blob))
            # A compatibility rewrite creates a separate file, leaving the source link intact.
            converted = models / "blobs/compat-migrate-test.gguf"
            converted.write_bytes(blob.read_bytes() + b" converted")
            self.assertEqual(source.read_bytes(), b"GGUF test")
            blob.unlink()
            self.assertEqual(source.read_bytes(), b"GGUF test")
            cli = root / "fail.py"
            cli.write_text("import sys; print('unsupported model', flush=True); sys.exit(7)")
            env = load_functions(models, lambda argv, **kw: subprocess.Popen([sys.executable, str(cli)], **kw))
            with self.assertRaisesRegex(RuntimeError, r"ollama create failed \(7\)"):
                env["_ollama_create"]("test", "not-used", str(source), every=1)

    def test_cross_device_dispatch_with_link_standin(self):
        # Exercises the EXDEV branch even on Windows without symlink privileges.
        # The stand-in is a hard link; this does not claim a native symlink test.
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            source = root / "model.gguf"
            source.write_bytes(b"GGUF immutable source")
            env = load_functions(root / "ollama")
            real_link, calls = os.link, []
            def symlink_standin(src, dst):
                calls.append((src, dst))
                real_link(src, dst)
            with patch.object(os, "link", side_effect=OSError(errno.EXDEV, "cross device")), \
                 patch.object(os, "symlink", side_effect=symlink_standin):
                self.assertTrue(env["_ollama_seed_blob"](str(source)))
            self.assertEqual(len(calls), 1)
            self.assertEqual(calls[0][0], os.path.realpath(source))
            self.assertEqual(Path(calls[0][1]).read_bytes(), source.read_bytes())

    def test_symlink_failure_preserves_source_and_falls_back(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            source = Path(temp) / "model.gguf"
            source.write_bytes(b"GGUF unchanged")
            models = Path(temp) / "ollama"
            env = load_functions(models)
            with patch.object(os, "link", side_effect=OSError(errno.EXDEV, "cross device")), \
                 patch.object(os, "symlink", side_effect=OSError(errno.EPERM, "symlinks unavailable")):
                self.assertFalse(env["_ollama_seed_blob"](str(source)))
            self.assertEqual(source.read_bytes(), b"GGUF unchanged")
            self.assertEqual(list((models / "blobs").iterdir()), [])

    def test_preexisting_blob_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            source = root / "model.gguf"
            source.write_bytes(b"GGUF test")
            blobs = root / "ollama/blobs"
            blobs.mkdir(parents=True)
            existing = blobs / ("sha256-" + hashlib.sha256(source.read_bytes()).hexdigest())
            existing.write_bytes(b"preexisting data")
            env = load_functions(root / "ollama")
            self.assertFalse(env["_ollama_seed_blob"](str(source)))
            self.assertEqual(existing.read_bytes(), b"preexisting data")

    def test_changed_source_is_not_seeded_under_an_old_digest(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            source = root / "model.gguf"
            source.write_bytes(b"GGUF initial")
            env = load_functions(root / "ollama")
            calls = 0

            def fstat(fd):
                nonlocal calls
                calls += 1
                st = os.fstat(fd)
                return types.SimpleNamespace(st_dev=st.st_dev, st_ino=st.st_ino, st_size=st.st_size,
                                             st_mtime_ns=st.st_mtime_ns + (1 if calls > 1 else 0))

            env["os"] = types.SimpleNamespace(path=os.path, makedirs=os.makedirs, stat=os.stat, fstat=fstat,
                                             link=os.link, unlink=os.unlink)
            self.assertFalse(env["_ollama_seed_blob"](str(source)))
            self.assertEqual(list((root / "ollama/blobs").iterdir()), [])

    def test_patch_upgrades_old_setup_and_is_idempotent(self):
        old = (ROOT / "kit/colab/setup.py").read_bytes()
        # Keep a legacy setup fixture even when this test runs after the optimization is committed.
        text = old.decode("utf-8")
        seed = next((n for n in ast.parse(text).body if isinstance(n, ast.FunctionDef) and n.name == "_ollama_seed_blob"), None)
        if seed:
            lines = text.splitlines(keepends=True)
            del lines[seed.lineno - 1:seed.end_lineno]
            text = "".join(lines).replace("    reused = _ollama_seed_blob(path, every)\n", "    reused = False\n")
            old = text.encode("utf-8")
        with tempfile.TemporaryDirectory() as temp:
            target = Path(temp) / "kit"
            shutil.copytree(ROOT / "kit", target, ignore=shutil.ignore_patterns("*.bak", "__pycache__"))
            (target / "colab/setup.py").write_bytes(old)
            cmd = [sys.executable, str(ROOT / "gateway/gate-ai.py"), str(target)]
            subprocess.run(cmd, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            once = (target / "colab/setup.py").read_bytes()
            self.assertIn(b"def _ollama_seed_blob(", once)
            subprocess.run(cmd, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            self.assertEqual((target / "colab/setup.py").read_bytes(), once)


if __name__ == "__main__":
    unittest.main()
