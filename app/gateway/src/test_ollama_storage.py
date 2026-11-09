"""Import peak-space accounting: mock mount/capacity only, no VM, large allocation or GPU."""
import ast
import contextlib
import io
from pathlib import Path
import subprocess
import types
import unittest

ROOT = Path(__file__).resolve().parents[2]
GB = 1_000_000_000


def fixture(size=48.7, disk=45, memory=125, used=48.7, store="/mnt/ram", existing=False, mount_rc=0):
    source = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
    names = {"_ollama_prepare_storage", "_ollama_check_import_space", "_ollama_env"}
    nodes = [n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name in names]
    assert len(nodes) == len(names)
    calls = []
    state = {"disk": disk, "memory": memory, "used": used, "total": 56}

    def usage(path):
        if path.startswith("/mnt/ram"):
            return types.SimpleNamespace(total=int(state["total"]*GB), free=int((state["total"]-state["used"])*GB))
        return types.SimpleNamespace(total=100*GB, free=int(state["disk"]*GB))

    def run(argv, **kw):
        calls.append(argv)
        assert argv[:2] == ["mount", "-o"] and argv[-1] == "/mnt/ram"
        assert kw["timeout"] == 30
        if not mount_rc:
            state["total"] = int(argv[2].split("=")[1][:-1]) * (1 << 20) / GB
        return types.SimpleNamespace(returncode=mount_rc)

    env = dict(PLAN={"models": [{"size_gb": size}], "ai": {"llm": {"ctx": 32768}}},
               STORE_ROOT=store, _WB_OLLAMA_MODELS="/content/ollama_models",
               mem_available=lambda: int(state["memory"]*GB), shutil=types.SimpleNamespace(disk_usage=usage),
               os=types.SimpleNamespace(environ={}, makedirs=lambda *a, **k: None,
                    listdir=lambda _: ["existing"] if existing else [], stat=lambda _: types.SimpleNamespace(st_dev=1),
                    path=types.SimpleNamespace(ismount=lambda p: p == "/mnt/ram", join=lambda *p: "/".join(p), getsize=lambda _: int(size*GB))),
               subprocess=types.SimpleNamespace(run=run, PIPE=subprocess.PIPE, STDOUT=subprocess.STDOUT, TimeoutExpired=subprocess.TimeoutExpired))
    exec(compile(ast.Module(body=nodes, type_ignores=[]), "<storage>", "exec"), env)
    return env, calls, state


class OllamaStorageTests(unittest.TestCase):
    def test_deepseek_ram_source_reuse_and_migration_reserve(self):
        env, calls, state = fixture()
        with contextlib.redirect_stdout(io.StringIO()):
            env["_ollama_prepare_storage"]()
            self.assertEqual(env["_ollama_env"]()["OLLAMA_MODELS"], "/mnt/ram/ollama_models")
            self.assertEqual(len(calls), 1)
            env["_ollama_check_import_space"]("source", True)
            # Failed linking must not proceed with a second source copy plus migration.
            with self.assertRaisesRegex(RuntimeError, "storage insufficient"):
                env["_ollama_check_import_space"]("source", False)
            state["memory"] = 50
            with self.assertRaisesRegex(RuntimeError, "RAM reserve insufficient"):
                env["_ollama_check_import_space"]("source", True)

    def test_qwen_keeps_disk_when_capacity_sufficient(self):
        env, calls, _ = fixture(size=29.05, used=29.05, disk=70)
        with contextlib.redirect_stdout(io.StringIO()):
            env["_ollama_prepare_storage"]()
            self.assertEqual(env["_ollama_env"]()["OLLAMA_MODELS"], "/content/ollama_models")
            env["_ollama_check_import_space"]("source", False)
        self.assertFalse(calls)

    def test_insufficient_ram_disk_profile_existing_store_or_failed_mount(self):
        for options in ({"memory": 50}, {"store": None}, {"existing": True}, {"mount_rc": 1}):
            with self.subTest(options=options), contextlib.redirect_stdout(io.StringIO()):
                env, calls, _ = fixture(**options)
                env["_ollama_prepare_storage"]()
                self.assertEqual(env["_ollama_env"]()["OLLAMA_MODELS"], "/content/ollama_models")
                with self.assertRaisesRegex(RuntimeError, "storage insufficient"):
                    env["_ollama_check_import_space"]("source", False)
                self.assertEqual(len(calls), 1 if "mount_rc" in options else 0)

    def test_disk_source_hard_link_still_reserves_conversion(self):
        env, _, state = fixture(store=None, disk=68.1)
        with contextlib.redirect_stdout(io.StringIO()):
            env["_ollama_prepare_storage"]()
            self.assertEqual(env["_ollama_env"]()["OLLAMA_MODELS"], "/content/ollama_models")
            env["_ollama_check_import_space"]("source", True)
            with self.assertRaisesRegex(RuntimeError, "storage insufficient"):
                env["_ollama_check_import_space"]("source", False)
            state["disk"] = 40
            with self.assertRaisesRegex(RuntimeError, "storage insufficient"):
                env["_ollama_check_import_space"]("source", True)


if __name__ == "__main__":
    unittest.main()
