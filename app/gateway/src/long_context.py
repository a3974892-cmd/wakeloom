"""Opt-in, pinned llama.cpp runtime for Qwen's YaRN 1M text profile.

Sources: Qwen/Qwen3.8-27B model card (YaRN factor 4, original 262144);
ggml-org/llama.cpp b12211 server README and official release asset digests.
Kept separate from the normal Ollama path; generated into setup.py by edits.py.
"""

SLOT_VALIDATION = r'''
def _long_slot_capacity(slots):
    # b12211 pads context to multiples of 256 (1,000,000 becomes 1,000,192).
    # Keep a single real slot and the advertised 1M minimum, not exact equality.
    if not isinstance(slots, list) or len(slots) != 1 or not isinstance(slots[0], dict):
        return 0
    capacity = slots[0].get("n_ctx")
    return capacity if type(capacity) is int and capacity >= 1000000 else 0
'''

RUNTIME_SOURCE = SLOT_VALIDATION + r'''
LLAMA_ROOT = "/content/wb_llama_b12211"
LLAMA_URL = "http://127.0.0.1:11435"
LLAMA_ASSETS = (
    ("llama-b12211-bin-ubuntu-cuda-12.8-x64.tar.gz", 188258232, 724566843,
     "103fac975e807da6cdb5717e87ca594e25bb7703dd8a7f10c22269c124e1a552"),
    ("cudart-llama-b12211-bin-ubuntu-cuda-12.8-x64.tar.gz", 594373633, 724560807,
     "d66dba34f0e45c66ce7d4484116c681aa71ff257ee1e47b02d9775aa59b5ef77"),
)

def llama_install():
    import hashlib
    import tarfile
    from pathlib import Path
    root = Path(LLAMA_ROOT)
    root.mkdir(parents=True, exist_ok=True)
    for name, size, asset_id, digest in LLAMA_ASSETS:
        archive = root / name
        last = None
        for attempt in range(1, 5):
            try:
                print(f"KIT_LONG_CONTEXT download {name} attempt {attempt}/4", flush=True)
                url = (f"https://api.github.com/repos/ggml-org/llama.cpp/releases/assets/{asset_id}" if attempt % 2 else
                       "https://github.com/ggml-org/llama.cpp/releases/download/b12211/" + name)
                req = urllib.request.Request(
                    url, headers={"User-Agent": "wakeloom-kit", "Accept": "application/octet-stream"})
                h, count, tick = hashlib.sha256(), 0, time.monotonic()
                with urllib.request.urlopen(req, timeout=60) as response, open(archive, "wb") as out:
                    while True:
                        chunk = response.read(4 << 20)
                        if not chunk:
                            break
                        count += len(chunk)
                        if count > size:
                            raise RuntimeError("llama.cpp archive exceeds expected size")
                        h.update(chunk)
                        out.write(chunk)
                        if time.monotonic() - tick >= 10:
                            print(f"KIT_LONG_CONTEXT download {count//1000000}/{size//1000000} MB", flush=True)
                            tick = time.monotonic()
                if count != size or h.hexdigest() != digest:
                    raise RuntimeError("llama.cpp archive size/SHA-256 mismatch")
                with tarfile.open(archive, "r:gz") as tf:
                    # Python's data filter validates paths and links within the install root.
                    tf.extractall(root, filter="data")
                archive.unlink()
                break
            except Exception as e:
                last = e
                archive.unlink(missing_ok=True)
                print(f"KIT_LONG_CONTEXT download failed: {type(e).__name__}: {e}", flush=True)
                if attempt == 4:
                    raise RuntimeError(f"llama.cpp install failed: {last}") from e
                time.sleep(attempt * 2)
    if not list(root.rglob("llama-server")):
        raise RuntimeError("verified archive did not contain llama-server")

def llama_command(binary, path, name):
    return [str(binary), "--model", path, "--alias", name,
            "--host", "127.0.0.1", "--port", "11435", "--ctx-size", "1000000",
            "--parallel", "1", "--n-gpu-layers", "999", "--flash-attn", "on",
            "--cache-type-k", "q8_0", "--cache-type-v", "q8_0",
            "--rope-scaling", "yarn", "--rope-scale", "4", "--yarn-orig-ctx", "262144",
            "--override-kv", "qwen35.context_length=int:1000000",
            "--batch-size", "2048", "--ubatch-size", "512", "--jinja",
            "--no-context-shift", "--fit", "off", "--slots"]

def llama_finish(llm):
    from pathlib import Path
    root = Path(LLAMA_ROOT)
    binary = next(root.rglob("llama-server"))
    binary.chmod(0o755)
    path = os.path.realpath(f"{COMFY}/models/{llm['dest']}")
    if not os.path.isfile(path):
        raise RuntimeError("1M model file missing")
    # No silent CPU fallback for a large 1M cache; normal profiles remain available.
    gpu = subprocess.check_output(["nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits"], text=True)
    if max(int(x.strip()) for x in gpu.splitlines() if x.strip()) < 80000:
        raise RuntimeError("1M experimental profile requires at least 80 GB GPU memory; choose the normal 32K profile")
    libdirs = sorted({str(p.parent) for p in root.rglob("*.so*")})
    env = {**os.environ, "LD_LIBRARY_PATH": ":".join(libdirs + [os.environ.get("LD_LIBRARY_PATH", "")])}
    for key in ("HF_TOKEN", "CF_TUNNEL_TOKEN"):
        env.pop(key, None)
    cmd = llama_command(binary, path, llm["name"])
    print("KIT_LONG_CONTEXT loading 1,000,000 tokens; YaRN x4; Q8 KV; one slot; experimental", flush=True)
    with open(f"{LOG}/llama.log", "a") as log:
        process = subprocess.Popen(cmd, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    for step in range(600):
        if process.poll() is not None:
            tail = Path(f"{LOG}/llama.log").read_text(errors="replace")[-6000:]
            raise RuntimeError("1M runtime exited during loading:\n" + tail)
        try:
            with urllib.request.urlopen(LLAMA_URL + "/slots", timeout=3) as response:
                slots = json.load(response)
        except Exception:
            if step % 5 == 0:
                print(f"KIT_LONG_CONTEXT loading: {step*2}s", flush=True)
            time.sleep(2)
            continue
        capacity = _long_slot_capacity(slots)
        observed = [slot.get("n_ctx") if isinstance(slot, dict) else None for slot in slots] if isinstance(slots, list) else None
        print(f"KIT_LONG_CONTEXT requested=1000000 slot_contexts={observed}", flush=True)
        if not capacity:
            process.terminate()
            raise RuntimeError(f"1M slot allocation mismatch: expected one slot >=1000000, received {observed}; refusing to report ready")
        break
    else:
        process.terminate()
        raise RuntimeError("1M runtime did not confirm a 1,000,000-token slot; see kit_logs/llama.log")
    # Short inference catches template/backend faults; not a 1M retrieval benchmark.
    req = urllib.request.Request(LLAMA_URL + "/v1/chat/completions", method="POST",
        data=json.dumps({"model": llm["name"], "messages": [{"role": "user", "content": "Reply OK"}],
                         "stream": False, "max_tokens": 16, "chat_template_kwargs": {"enable_thinking": False}}).encode(),
        headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=180) as response:
            result = json.load(response)
        if not result.get("choices") or not result["choices"][0].get("message", {}).get("content"):
            raise RuntimeError("1M short inference check returned no text")
    except Exception:
        process.terminate()
        raise
    with open(AI_INFO) as f:
        info = json.load(f)
    info.update(ready=True, context_verified=True, context_tokens=1000000, allocated_context_tokens=capacity, vision=False)
    with open(AI_INFO, "w") as f:
        json.dump(info, f)
    print(f"KIT_LONG_CONTEXT allocated={capacity} advertised=1000000; short inference passed; full-window quality untested", flush=True)
'''

NODE_SOURCE = SLOT_VALIDATION + r'''
# Restricted text-only llama.cpp adapter; normal Ollama proxy remains unchanged.
LLAMA = "http://127.0.0.1:11435"
LONG_OUTPUT = 16384

def _long_info():
    try:
        with open("/content/wb_ai.json") as f:
            return json.load(f)
    except Exception:
        return {}

def _long_messages(body):
    messages = body.get("messages") or []
    if not isinstance(messages, list) or not messages:
        raise ValueError("messages required")
    out = []
    for message in messages:
        if message.get("images"):
            raise ValueError("1M experimental profile is text-only; use the vision profile for images")
        if message.get("role") not in ("system", "user", "assistant") or not isinstance(message.get("content"), str):
            raise ValueError("invalid text message")
        out.append({"role": message["role"], "content": message["content"]})
    return out

async def _long_post(session, path, body):
    async with session.post(LLAMA + path, json=body) as response:
        if response.status != 200:
            raise RuntimeError(f"1M service {path}: HTTP {response.status}: " + (await response.text())[:500])
        return await response.json()

async def _long_count(session, messages, think):
    rendered = await _long_post(session, "/apply-template", {
        "messages": messages, "chat_template_kwargs": {"enable_thinking": think}})
    if not isinstance(rendered.get("prompt"), str):
        raise RuntimeError("1M template service returned no prompt")
    encoded = await _long_post(session, "/tokenize", {"content": rendered["prompt"], "add_special": True, "parse_special": True})
    if not isinstance(encoded.get("tokens"), list):
        raise RuntimeError("1M tokenizer returned no token count")
    return len(encoded["tokens"])

async def _wb_long_proxy(request, tail):
    import asyncio
    info = _long_info()
    name = (info.get("llm") or {}).get("name")
    if tail == "api/show":
        return web.json_response({"capabilities": ["completion"], "model_info": {"qwen35.context_length": 1000000}})
    if tail not in ("api/chat", "api/context-check") or request.method != "POST":
        return web.json_response({"error": "not allowed in 1M profile"}, status=404)
    try:
        body = await request.json()
        if body.get("model") != name or not info.get("context_verified"):
            raise ValueError("1M runtime/model is not verified; wake the 1M profile again")
        messages = _long_messages(body)
        think = bool(body.get("think", False))
        opts = body.get("options") or {}
        if int(opts.get("num_ctx", 1000000)) != 1000000:
            raise ValueError("1M profile requires num_ctx=1000000")
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=None, sock_connect=5, sock_read=3600)) as session:
            # Verify live allocation as well as the startup report (no stale 1M label).
            async with session.get(LLAMA + "/slots") as response:
                slots = await response.json()
            if not _long_slot_capacity(slots):
                raise RuntimeError("1M slot allocation is no longer available")
            count = await _long_count(session, messages, think)
            budget = 1000000 - LONG_OUTPUT - 4096
            if tail == "api/context-check":
                return web.json_response({"tokens": count, "input_budget": budget, "context_tokens": 1000000})
            if count > budget:
                return web.json_response({"error": f"输入 {count} tokens，超过本次预算 {budget}；请缩短文字或附件。"}, status=400)
            payload = {"model": name, "messages": messages, "stream": True,
                       "max_tokens": LONG_OUTPUT, "temperature": opts.get("temperature", 0.7),
                       "top_p": 0.95 if think else 0.8, "top_k": 20, "min_p": 0.0,
                       "presence_penalty": 0.0 if think else 1.5, "repeat_penalty": 1.0,
                       "chat_template_kwargs": {"enable_thinking": think},
                       "stream_options": {"include_usage": True}}
            if body.get("format") == "json":
                payload["response_format"] = {"type": "json_object"}
            out = web.StreamResponse(headers={"Content-Type": "application/x-ndjson", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"})
            await out.prepare(request)
            # Send headers before prefill; keep the proxy connection alive while the
            # GPU processes a large prompt. Empty JSON is ignored by existing clients.
            owner = asyncio.current_task()
            async def heartbeat():
                try:
                    while True:
                        await asyncio.sleep(15)
                        await out.write(b"{}\n")
                except (ConnectionResetError, RuntimeError):
                    owner.cancel()
            pulse = asyncio.create_task(heartbeat())
            try:
                await out.write((json.dumps({"wb_tokens": {"input": count, "window": 1000000, "output_reserved": LONG_OUTPUT}}) + "\n").encode())
                async with session.post(LLAMA + "/v1/chat/completions", json=payload) as response:
                    if response.status != 200:
                        raise RuntimeError(f"1M inference HTTP {response.status}: " + (await response.text())[:1000])
                    usage = {}
                    ended = False
                    async for line in response.content:
                        if not line.startswith(b"data: "):
                            continue
                        value = line[6:].strip()
                        if value == b"[DONE]":
                            ended = True
                            break
                        event = json.loads(value)
                        if event.get("error"):
                            raise RuntimeError(str(event["error"])[:1000])
                        if event.get("usage"):
                            usage = event["usage"]
                        for choice in event.get("choices", []):
                            delta = choice.get("delta") or {}
                            if delta.get("content") or delta.get("reasoning_content"):
                                await out.write((json.dumps({"message": {"content": delta.get("content", ""), "thinking": delta.get("reasoning_content", "")}}) + "\n").encode())
                    if not ended:
                        raise RuntimeError("1M response stream ended early; see diagnostic logs")
                    await out.write((json.dumps({"done": True, "eval_count": usage.get("completion_tokens", 0)}) + "\n").encode())
            except (ConnectionResetError, asyncio.CancelledError):
                raise
            except Exception as e:
                await out.write((json.dumps({"error": str(e)[:1000]}) + "\n").encode())
            finally:
                pulse.cancel()
                try:
                    await pulse
                except asyncio.CancelledError:
                    pass
            await out.write_eof()
            return out
    except ValueError as e:
        return web.json_response({"error": str(e)}, status=400)
    except Exception as e:
        return web.json_response({"error": "1M 服务错误：" + str(e)[:1000]}, status=502)
'''
