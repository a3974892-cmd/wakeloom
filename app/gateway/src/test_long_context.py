"""1M adapter integration tests with loopback llama mocks; no GPU/downloads.

Requires aiohttp (also a ComfyUI dependency). Run after regenerating the kit.
"""
import ast
import asyncio
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch
import urllib.request

import aiohttp
from aiohttp import web
from aiohttp.test_utils import TestServer, TestClient
from long_context import RUNTIME_SOURCE, NODE_SOURCE

ROOT = Path(__file__).resolve().parents[2]


class LongRuntimeTests(unittest.TestCase):
    def test_slot_capacity_accepts_padding_not_missing_or_insufficient_capacity(self):
        for source in (RUNTIME_SOURCE, NODE_SOURCE):
            env = {}
            exec(source, env)
            check = env["_long_slot_capacity"]
            for size in (1000000, 1000192, 1048576):
                self.assertEqual(check([{"n_ctx": size}]), size)
            for slots in (None, {}, [], [None], [{}], [{"n_ctx":999999}],
                          [{"n_ctx":32768}], [{"n_ctx":"1000000"}],
                          [{"n_ctx":True}], [{"n_ctx":1000000.0}],
                          [{"n_ctx":1000000}, {"n_ctx":1000000}]):
                self.assertEqual(check(slots), 0)

    def test_generated_sources_and_exact_runtime_flags(self):
        source = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
        self.assertIn(RUNTIME_SOURCE.strip(), source)
        module = ast.parse(source)
        node = next(ast.literal_eval(n.value) for n in module.body
                    if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "AI_NODE" for t in n.targets))
        self.assertIn(NODE_SOURCE.strip(), node)
        compile(node, "<generated-node>", "exec")
        env = {}
        exec(RUNTIME_SOURCE, env)
        args = env["llama_command"]("server", "weights.gguf", "long")
        for flag, value in {"--ctx-size":"1000000", "--parallel":"1", "--rope-scaling":"yarn",
                            "--rope-scale":"4", "--yarn-orig-ctx":"262144", "--fit":"off",
                            "--cache-type-k":"q8_0", "--cache-type-v":"q8_0"}.items():
            self.assertEqual(args[args.index(flag)+1], value)
        self.assertIn("--no-context-shift", args)
        self.assertIn("--slots", args)

    def test_installer_rejects_bad_digest_and_escaping_archive(self):
        for scenario in ("good", "digest", "traversal"):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stdout(io.StringIO()):
                stream = io.BytesIO()
                with tarfile.open(fileobj=stream, mode="w:gz") as tf:
                    entry = tarfile.TarInfo("../escape" if scenario == "traversal" else "bin/llama-server")
                    entry.size = 4
                    tf.addfile(entry, io.BytesIO(b"test"))
                data = stream.getvalue()
                calls = []
                def urlopen(req, **kwargs):
                    calls.append(req.full_url)
                    return io.BytesIO(data)
                env = {"urllib": types.SimpleNamespace(request=types.SimpleNamespace(Request=urllib.request.Request, urlopen=urlopen)),
                       "time": types.SimpleNamespace(monotonic=lambda: 0, sleep=lambda _: None)}
                exec(RUNTIME_SOURCE, env)
                env["LLAMA_ROOT"] = str(Path(tmp) / "runtime")
                env["LLAMA_ASSETS"] = (("mock.tar.gz", len(data), 123, "0"*64 if scenario == "digest" else hashlib.sha256(data).hexdigest()),)
                if scenario == "good":
                    env["llama_install"]()
                    self.assertEqual(len(calls), 1)
                    self.assertTrue((Path(tmp) / "runtime/bin/llama-server").is_file())
                else:
                    with self.assertRaises(RuntimeError): env["llama_install"]()
                    self.assertEqual(len(calls), 4)
                    self.assertFalse((Path(tmp) / "escape").exists())
                    self.assertFalse((Path(tmp) / "runtime/bin/llama-server").exists())

    def test_profile_parser_is_opt_in(self):
        source = (ROOT / "kit/gate/profiles.py").read_text(encoding="utf-8")
        fn = next(n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name == "_parse_ai")
        env = dict(AI_TYPES=("llm", "image"), Any=object, ProfileError=ValueError, re=__import__("re"), _req=lambda obj, key, ctx: obj[key])
        exec(compile(ast.Module(body=[fn], type_ignores=[]), "<parser>", "exec"), env)
        models = {"m":types.SimpleNamespace(dest="llm/Huihui-Qwen3.8-27B-abliterated-Q8_0.gguf")}
        raw = {"type":"llm", "llm":{"model":"m", "ctx":1000000, "engine":"llama_cpp_1m"}}
        self.assertEqual(env["_parse_ai"](raw, models, "test")["llm"]["ctx"], 1000000)
        raw["llm"]["projector"] = "vision"
        with self.assertRaises(ValueError): env["_parse_ai"](raw, models, "test")
        raw["llm"] = {"model":"m"}
        parsed = env["_parse_ai"](raw, models, "test")["llm"]
        self.assertEqual(parsed["ctx"], 32768)
        self.assertNotIn("engine", parsed)

    def test_startup_requires_memory_slot_and_inference(self):
        for scenario in ("ready", "padded", "low-memory", "wrong-slot", "inference-failed", "process-exit"):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stdout(io.StringIO()):
                root = Path(tmp)
                (root / "llama-server").write_text("mock")
                (root / "models").mkdir()
                (root / "models/model.gguf").write_text("mock")
                (root / "info.json").write_text("{}")
                stopped, started = [], []
                process = types.SimpleNamespace(poll=lambda: 1 if scenario == "process-exit" else None, terminate=lambda: stopped.append(True))
                def start(*args, **kwargs):
                    started.append(args)
                    return process
                def urlopen(req, **kwargs):
                    url = req if isinstance(req,str) else req.full_url
                    if url.endswith("/slots"):
                        return io.BytesIO(json.dumps([{"n_ctx":32768 if scenario == "wrong-slot" else 1000192 if scenario == "padded" else 1000000}]).encode())
                    return io.BytesIO(json.dumps({"choices":[] if scenario == "inference-failed" else [{"message":{"content":"OK"}}]}).encode())
                env = dict(os=os, json=json, COMFY=tmp, LOG=tmp, AI_INFO=str(root/"info.json"),
                           subprocess=types.SimpleNamespace(check_output=lambda *a,**k: "23034" if scenario == "low-memory" else "97887", Popen=start, STDOUT=-2),
                           urllib=types.SimpleNamespace(request=types.SimpleNamespace(Request=urllib.request.Request,urlopen=urlopen)))
                exec(RUNTIME_SOURCE, env)
                env["LLAMA_ROOT"] = tmp
                if scenario in ("ready", "padded"):
                    env["llama_finish"]({"dest":"model.gguf","name":"long"})
                    self.assertEqual(json.loads((root/"info.json").read_text())["context_tokens"],1000000)
                    self.assertEqual(json.loads((root/"info.json").read_text())["allocated_context_tokens"],1000192 if scenario == "padded" else 1000000)
                    self.assertFalse(stopped)
                else:
                    with self.assertRaises(RuntimeError): env["llama_finish"]({"dest":"model.gguf","name":"long"})
                    self.assertNotIn("ready", json.loads((root/"info.json").read_text()))
                    if scenario == "low-memory": self.assertFalse(started)
                    if scenario in ("wrong-slot","inference-failed"): self.assertTrue(stopped)


class LongAdapterTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.count = 10
        self.slot = 1000000
        self.mode = "ok"
        self.received = []
        self.release = asyncio.Event()
        self.started = asyncio.Event()
        async def mock(request):
            path = request.path
            body = await request.json() if request.method == "POST" else None
            self.received.append((path,body))
            if path == "/slots": return web.json_response([{"n_ctx":self.slot}])
            if path == "/apply-template":
                if self.mode == "token-error": return web.json_response({"error":"unavailable"},status=503)
                return web.json_response({"prompt":"<template>"+body["messages"][0]["content"]})
            if path == "/tokenize": return web.json_response({"tokens":[1]*self.count})
            if path == "/v1/chat/completions":
                self.started.set()
                if self.mode == "delayed": await self.release.wait()
                if self.mode == "http-error": return web.json_response({"error":"inference failed"},status=500)
                out = web.StreamResponse(headers={"Content-Type":"text/event-stream"})
                await out.prepare(request)
                for event in [{"choices":[{"delta":{"reasoning_content":"reason"}}]},
                              {"choices":[{"delta":{"content":"answer"}}]}, {"usage":{"completion_tokens":2}}]:
                    await out.write(("data: "+json.dumps(event)+"\n\n").encode())
                if self.mode != "broken": await out.write(b"data: [DONE]\n\n")
                await out.write_eof()
                return out
            return web.json_response({},status=404)
        app = web.Application(client_max_size=32<<20)
        app.router.add_route("*","/{path:.*}",mock)
        self.upstream = TestServer(app)
        await self.upstream.start_server()
        source = (ROOT / "kit/colab/setup.py").read_text(encoding="utf-8")
        module = ast.parse(source)
        node = next(ast.literal_eval(n.value) for n in module.body
                    if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "AI_NODE" for t in n.targets))
        routes = web.RouteTableDef()
        server = types.SimpleNamespace(PromptServer=types.SimpleNamespace(instance=types.SimpleNamespace(routes=routes)))
        self.env = {}
        with patch.dict(sys.modules, {"server": server}): exec(node, self.env)
        self.info = {"context_verified":True,"llm":{"name":"long","engine":"llama_cpp_1m"}}
        self.env["_long_info"] = lambda: self.info
        self.env["LLAMA"] = str(self.upstream.make_url("/")).rstrip("/")
        adapter = web.Application()
        adapter.add_routes(routes)
        self.client = TestClient(TestServer(adapter))
        await self.client.start_server()

    async def asyncTearDown(self):
        self.release.set()
        await self.client.close()
        await self.upstream.close()

    def body(self, **kwargs):
        return dict(model="long", messages=[{"role":"user","content":"hello"}], think=False, options={"num_ctx":1000000}, **kwargs)

    async def test_large_body_token_count_and_stream_translation(self):
        body = self.body()
        body["messages"][0]["content"] = "x" * (2<<20)
        response = await self.client.post("/wbllm/api/context-check", json=body)
        self.assertEqual(response.status, 200)
        self.assertEqual(await response.json(), {"tokens":10,"input_budget":979520,"context_tokens":1000000})
        rendered = next(b for p,b in self.received if p == "/apply-template")
        self.assertFalse(rendered["chat_template_kwargs"]["enable_thinking"])
        encoded = next(b for p,b in self.received if p == "/tokenize")
        self.assertTrue(encoded["content"].startswith("<template>"))
        self.assertTrue(encoded["add_special"] and encoded["parse_special"])
        response = await self.client.post("/wbllm/api/chat",json=self.body(format="json"))
        events = [json.loads(x) for x in (await response.text()).splitlines()]
        self.assertEqual(events[-1],{"done":True,"eval_count":2})
        self.assertEqual(events[1]["message"]["thinking"],"reason")
        self.assertEqual(events[2]["message"]["content"],"answer")
        sent = next(b for p,b in self.received if p == "/v1/chat/completions")
        self.assertEqual(sent["max_tokens"],16384)
        self.assertEqual(sent["response_format"], {"type":"json_object"})
        self.assertFalse(sent["chat_template_kwargs"]["enable_thinking"])

    async def test_guards_prevent_inference(self):
        for scenario in ("wrong-model", "unverified", "images", "wrong-slot", "token-error", "over-budget"):
            with self.subTest(scenario=scenario):
                self.info["context_verified"] = scenario != "unverified"
                self.slot = 32768 if scenario == "wrong-slot" else 1000000
                self.mode = scenario
                self.count = 979521 if scenario == "over-budget" else 10
                body = self.body()
                if scenario == "wrong-model": body["model"] = "other"
                if scenario == "images": body["messages"][0]["images"] = ["abc"]
                response = await self.client.post("/wbllm/api/chat", json=body)
                self.assertGreaterEqual(response.status,400)
                self.assertIn("error", await response.json())
                self.assertFalse(self.started.is_set())

    async def test_aligned_slot_remains_usable_without_expanding_chat_budget(self):
        self.slot = 1000192
        response = await self.client.post("/wbllm/api/context-check", json=self.body())
        self.assertEqual(response.status, 200)
        self.assertEqual(await response.json(), {"tokens":10,"input_budget":979520,"context_tokens":1000000})
        response = await self.client.post("/wbllm/api/chat", json=self.body())
        self.assertEqual(response.status, 200)
        self.assertIn('"done": true', await response.text())

    async def test_errors_after_headers_never_report_success(self):
        for mode in ("http-error", "broken"):
            self.mode = mode
            response = await self.client.post("/wbllm/api/chat",json=self.body())
            events = [json.loads(x) for x in (await response.text()).splitlines()]
            self.assertIn("error",events[-1])
            self.assertFalse(any(e.get("done") for e in events))

    async def test_headers_arrive_before_long_prefill(self):
        self.mode = "delayed"
        response = await asyncio.wait_for(self.client.post("/wbllm/api/chat",json=self.body()),2)
        first = await asyncio.wait_for(response.content.readline(),2)
        self.assertIn("wb_tokens",json.loads(first))
        await asyncio.wait_for(self.started.wait(),2)
        self.release.set()
        self.assertIn('"done": true',await response.text())


if __name__ == "__main__": unittest.main()
