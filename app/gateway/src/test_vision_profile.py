"""Optional projector boot checks; no network, Ollama process or GPU required."""
import ast
import builtins
import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import types
import unittest
import urllib.request

ROOT = Path(__file__).resolve().parents[2]


class VisionProfileTests(unittest.TestCase):
    def test_projector_registration_and_capability_gate(self):
        source = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
        func = next(n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name == "ai_finish")
        code = compile(ast.Module(body=[func], type_ignores=[]), "<ai_finish>", "exec")
        for case in ("text", "vision", "missing", "empty", "unsupported"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
                root = Path(temp)
                models = root / "models/llm"
                models.mkdir(parents=True)
                (models / "model.gguf").write_bytes(b"test-main")
                if case not in ("text", "missing"):
                    (models / "mmproj.gguf").write_bytes(b"" if case == "empty" else b"test-projector")
                info = root / "ai.json"
                info.write_text("{}")
                llm = dict(dest="llm/model.gguf", name="test-model")
                if case != "text":
                    llm["projector_dest"] = "llm/mmproj.gguf"
                requests, registered, seeded = [], [], []

                def urlopen(req, **kwargs):
                    requests.append(req.full_url)
                    if req.full_url.endswith("/api/show"):
                        return io.BytesIO(json.dumps({"capabilities": ["completion"] + (["vision"] if case == "vision" else [])}).encode())
                    return io.BytesIO(b"{}")

                def open_file(name, *args, **kwargs):
                    if str(name) == "/content/wb_Modelfile":
                        name = root / "Modelfile"
                    return builtins.open(name, *args, **kwargs)

                def create(name, mf, path):
                    registered.append((root / "Modelfile").read_text())

                env = dict(PLAN={"ai": {"type": "llm", "llm": llm}}, COMFY=temp, AI_INFO=str(info),
                           os=os, json=json, open=open_file, Stage=lambda _: contextlib.nullcontext(),
                           _ollama_seed_blob=seeded.append, _ollama_create=create, OLLAMA_URL="http://mock",
                           urllib=types.SimpleNamespace(request=types.SimpleNamespace(Request=urllib.request.Request, urlopen=urlopen)),
                           _ollama_bin=lambda: "ollama", _ollama_env=lambda: {}, sh=lambda *a, **k: None)
                exec(code, env)
                thread = types.SimpleNamespace(join=lambda _: None, is_alive=lambda: False)
                if case in ("text", "vision"):
                    env["ai_finish"]((thread, {}))
                    self.assertTrue(json.loads(info.read_text())["ready"])
                    self.assertEqual(len(registered), 1)
                    self.assertEqual(registered[0].count("FROM "), 1 if case == "text" else 2)
                    self.assertEqual(len(seeded), 0 if case == "text" else 1)
                    self.assertEqual(any(url.endswith("/api/show") for url in requests), case == "vision")
                else:
                    with self.assertRaises(RuntimeError):
                        env["ai_finish"]((thread, {}))
                    self.assertNotIn("ready", json.loads(info.read_text()))
                    self.assertFalse(any(url.endswith("/api/generate") for url in requests))

    def test_parser_requires_separate_core_projector(self):
        source = (ROOT / "kit/gate/profiles.py").read_text(encoding="utf-8")
        func = next(n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name == "_parse_ai")
        env = dict(AI_TYPES=("llm", "image"), Any=object, ProfileError=ValueError, re=__import__("re"),
                   _req=lambda obj, key, ctx: obj[key])
        exec(compile(ast.Module(body=[func], type_ignores=[]), "<parse_ai>", "exec"), env)
        models = {"main": types.SimpleNamespace(dest="llm/model.gguf"), "vision": types.SimpleNamespace(dest="llm/mmproj.gguf")}
        raw = {"type": "llm", "llm": {"model": "main", "projector": "vision"}}
        result = env["_parse_ai"](raw, models, "test")
        self.assertEqual(result["models"], ["main", "vision"])
        self.assertEqual(result["llm"]["projector_dest"], "llm/mmproj.gguf")
        for bad in ("main", "missing"):
            raw["llm"]["projector"] = bad
            with self.assertRaises(ValueError):
                env["_parse_ai"](raw, models, "test")


if __name__ == "__main__":
    unittest.main()
