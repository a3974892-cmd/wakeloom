"""Bootstrap compatibility and subprocess failure/retry checks; no GPU or model downloads."""
import ast
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
import urllib.error
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


def helper_env(root):
    # Exercise the shared helpers independently of a real Colab session.
    generated = ast.parse((ROOT / "gateway/gate-bootstrap.py").read_text(encoding="utf-8"))
    helpers = next(ast.literal_eval(n.value) for n in generated.body if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "HELPERS" for t in n.targets))
    tree = ast.parse(helpers)
    nodes = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name.startswith("_wb_bootstrap")]
    env = dict(os=os, re=re, sys=sys, time=time, signal=signal, subprocess=subprocess, shutil=shutil,
               threading=threading, LOG=str(root / "logs"), COMFY=str(root / "ComfyUI"))
    exec(compile(ast.Module(body=nodes, type_ignores=[]), "<bootstrap>", "exec"), env)
    return env


class FastEvent(threading.Event):
    def wait(self, timeout=None):
        return self.is_set()


class BootstrapTests(unittest.TestCase):
    def test_verified_download_falls_back_after_504_and_hides_signed_url(self):
        data = b"!<arch>\nverified package fixture"
        response = io.BytesIO(data)
        response.status = 200
        output = io.StringIO()
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(output):
            root = Path(temp)
            env = helper_env(root)
            dest = root / "cf.deb"
            failure = urllib.error.HTTPError("https://example.invalid/?secret=do-not-log", 504, "gateway", {}, None)
            with patch("urllib.request.urlopen", side_effect=[failure, response]) as fetch:
                env["_wb_bootstrap_download"](["https://api.invalid/asset", "https://release.invalid/package"],
                    str(dest), FastEvent(), len(data), hashlib.sha256(data).hexdigest())
            self.assertEqual(dest.read_bytes(), data)
            self.assertEqual(fetch.call_count, 2)
            self.assertEqual(fetch.call_args.args[0].get_header("Accept"), "application/octet-stream")
        self.assertIn("HTTP 504", output.getvalue())
        self.assertIn("verified", output.getvalue())
        self.assertNotIn("do-not-log", output.getvalue())

    def test_invalid_download_never_leaves_installable_package(self):
        for payload, status in [(b"bad", 200), (b"too-large" * 10, 200), (b"good", 206)]:
            with self.subTest(status=status, payload=payload), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                env = helper_env(root)
                dest = root / "cf.deb"
                def response(*args, **kwargs):
                    r = io.BytesIO(payload)
                    r.status = status
                    return r
                with contextlib.redirect_stdout(io.StringIO()), patch("urllib.request.urlopen", side_effect=response) as fetch:
                    with self.assertRaisesRegex(RuntimeError, "official routes failed"):
                        env["_wb_bootstrap_download"](["https://api.invalid", "https://release.invalid"],
                            str(dest), FastEvent(), 4, hashlib.sha256(b"good").hexdigest())
                self.assertEqual(fetch.call_count, 4)
                self.assertFalse(dest.exists())

    def test_download_cancel_and_timeout_do_not_start_more_requests(self):
        with tempfile.TemporaryDirectory() as temp:
            env = helper_env(Path(temp))
            cancel = FastEvent()
            cancel.set()
            with patch("urllib.request.urlopen") as fetch:
                with self.assertRaisesRegex(RuntimeError, "canceled"):
                    env["_wb_bootstrap_download"](["https://api.invalid"], str(Path(temp)/"cf"), cancel, 1, "unused")
                self.assertFalse(fetch.called)
            with patch.object(time, "monotonic", side_effect=[0, 181]), patch("urllib.request.urlopen") as fetch:
                with self.assertRaisesRegex(RuntimeError, "total timeout"):
                    env["_wb_bootstrap_download"](["https://api.invalid"], str(Path(temp)/"cf"), FastEvent(), 1, "unused")
                self.assertFalse(fetch.called)

    def test_cloudflared_existing_binary_and_failed_download_skip_dpkg(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            env = helper_env(Path(temp))
            calls = []
            env["_wb_bootstrap_command"] = lambda *a, **k: calls.append(a)
            env["_wb_bootstrap_download"] = lambda *a: (_ for _ in ()).throw(RuntimeError("download rejected"))
            with patch.object(shutil, "which", return_value="/usr/bin/cloudflared"):
                env["_wb_bootstrap_cloudflared"](FastEvent())
            with patch.object(shutil, "which", return_value=None):
                with self.assertRaisesRegex(RuntimeError, "download rejected"):
                    env["_wb_bootstrap_cloudflared"](FastEvent())
            self.assertEqual(calls, [])

    def test_optional_patch_preserves_all_non_bootstrap_functions_and_model_plan(self):
        spec = importlib.util.spec_from_file_location("standalone_bootstrap", ROOT / "gateway/gate-bootstrap.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        embedded = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
        # Reconstruct the previously shipped bootstrap to retain real upgrade coverage.
        original = embedded.replace(module.HELPERS.strip(), "").replace(
            module.MAIN, module.SUPPORTED[0] + module.END)
        updated = module.patch_source(original)
        def functions(text):
            return {n.name: ast.dump(n, include_attributes=False) for n in ast.parse(text).body if isinstance(n, ast.FunctionDef)}
        before, after = functions(original), functions(updated)
        for name, tree in before.items():
            if name != "main":
                self.assertEqual(after[name], tree, name)
        old_main = ast.parse(original).body
        new_main = ast.parse(updated).body
        def rest(nodes):
            n = next(n for n in nodes if isinstance(n, ast.FunctionDef) and n.name == "main")
            n.body = [x for x in n.body if not (isinstance(x, ast.With) and any(isinstance(i.context_expr, ast.Call) and
                          isinstance(i.context_expr.func, ast.Name) and i.context_expr.func.id == 'Stage' and
                          i.context_expr.args and isinstance(i.context_expr.args[0], ast.Constant) and
                          i.context_expr.args[0].value == 'bootstrap' for i in x.items))]
            return ast.dump(n, include_attributes=False)
        self.assertEqual(rest(old_main), rest(new_main))
        self.assertNotIn("def _wb_bootstrap(", original)
        self.assertEqual(ast.dump(ast.parse(updated)), ast.dump(ast.parse(embedded)))
        self.assertEqual(module.patch_source(embedded), embedded)

    def test_real_child_stderr_retry_and_redaction(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            env = helper_env(root)
            marker = root / "retry"
            code = "import pathlib,sys; p=pathlib.Path(sys.argv[1]); first=not p.exists(); p.write_text('done'); print('network '+sys.argv[2],file=sys.stderr); sys.exit(7 if first else 0)"
            output = io.StringIO()
            token = "hf_unit_test_secret_only"
            with patch.dict(os.environ, {"HF_TOKEN": token}), contextlib.redirect_stdout(output):
                env["_wb_bootstrap_command"]("network-test", [sys.executable, "-c", code, str(marker), token], FastEvent(), attempts=2)
            self.assertIn("exit 7", output.getvalue())
            self.assertIn("attempt 2/2", output.getvalue())
            self.assertIn("[redacted]", output.getvalue())
            self.assertNotIn(token, output.getvalue())

    def test_timeout_and_cancellation_are_bounded(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            env = helper_env(Path(temp))
            start = time.monotonic()
            with self.assertRaisesRegex(RuntimeError, "timeout"):
                env["_wb_bootstrap_command"]("slow", [sys.executable, "-c", "import time;time.sleep(30)"], FastEvent(), timeout=0.15)
            self.assertLess(time.monotonic() - start, 4)
            event = FastEvent()
            event.set()
            with self.assertRaisesRegex(RuntimeError, "canceled"):
                env["_wb_bootstrap_command"]("cancel", ["must-not-start"], event)

    def test_clone_retries_preserve_downloads_and_exact_checkout(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            env = helper_env(root)
            model = root / "ComfyUI/models/llm/original.gguf"
            model.parent.mkdir(parents=True)
            model.write_bytes(b"existing downloaded bytes")
            calls, checkouts = [], []

            def command(label, argv, *args, **kwargs):
                calls.append((label, argv))
                if label.startswith("comfyui-clone"):
                    dest = Path(argv[-1])
                    dest.mkdir()
                    checkouts.append(dest)
                    if len(checkouts) == 1:
                        (dest / "partial").write_text("partial clone")
                        raise RuntimeError("temporary network failure")
                    (dest / "main.py").write_text("fixture")
                    (dest / "models/llm").mkdir(parents=True)
                    (dest / "models/llm/put_models_here").write_text("placeholder")
                elif label == "comfyui-checkout":
                    self.assertEqual(argv[-1], "pinned-commit")
                elif label == "comfyui-copy":
                    shutil.copytree(Path(argv[-2]), Path(argv[-1]), dirs_exist_ok=True, symlinks=True)

            env["_wb_bootstrap_command"] = command
            env["_wb_bootstrap_comfy"]({"repo": "https://example.invalid/original", "commit": "pinned-commit"}, FastEvent())
            self.assertEqual(len(checkouts), 2)
            self.assertTrue(all(not p.parent.exists() for p in checkouts))
            self.assertEqual(model.read_bytes(), b"existing downloaded bytes")
            self.assertTrue((root / "ComfyUI/main.py").is_file())

    def test_parallel_failure_cancels_other_jobs_and_uv_keeps_pip_fallback(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            env = helper_env(Path(temp))
            env["_wb_bootstrap_command"] = lambda *a, **k: (_ for _ in ()).throw(RuntimeError("uv unavailable"))
            env["_wb_bootstrap_uv"](FastEvent())  # original pip fallback remains usable
            stopped = []

            def fail(cancel):
                raise RuntimeError("cloudflared-download failed")

            def wait(*args):
                cancel = args[-1]
                while not cancel.is_set():
                    time.sleep(0.001)
                stopped.append(True)

            env.update(_wb_bootstrap_cloudflared=fail, _wb_bootstrap_comfy=wait, _wb_bootstrap_uv=wait)
            with self.assertRaisesRegex(RuntimeError, "cloudflared-download"):
                env["_wb_bootstrap"]({})
            self.assertEqual(len(stopped), 2)

    def test_standalone_patch_backup_refusal_and_mount_discovery(self):
        spec = importlib.util.spec_from_file_location("bootstrap_patch", ROOT / "gateway/gate-bootstrap.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        legacy = "def untouched():\n    return 'preserved'\n\ndef main():\n    cfg = {}\n    dl = object()\n" + module.SUPPORTED[0] + '\n    with Stage("deps"):\n        pass\n'
        updated = module.patch_source(legacy)
        self.assertEqual(module.patch_source(updated), updated)
        self.assertEqual(ast.dump(ast.parse(legacy).body[0]), ast.dump(ast.parse(updated).body[0]))
        with self.assertRaisesRegex(RuntimeError, "Custom bootstrap"):
            module.patch_source(legacy.replace("procs = []", "procs = []\n        custom_user_step()"))
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            path = Path(temp) / "setup.py"
            path.write_text(legacy, encoding="utf-8")
            module.install(path, check=True)
            self.assertEqual(path.read_text(encoding="utf-8"), legacy)
            module.install(path)
            module.install(path)
            backups = list(path.parent.glob("*.bak"))
            self.assertEqual(len(backups), 1)
            self.assertEqual(backups[0].read_text(encoding="utf-8"), legacy)
            mounts = [{"Type": "bind", "Source": temp, "Destination": "/app/packages/colab"}]
            with patch.object(module.subprocess, "run", return_value=types.SimpleNamespace(stdout=json.dumps(mounts))) as run:
                self.assertEqual(module.discover(), path.resolve())
                self.assertEqual(run.call_args.args[0], ["docker", "inspect", "--format", "{{json .Mounts}}", "comfy-gate"])


if __name__ == "__main__":
    unittest.main()
