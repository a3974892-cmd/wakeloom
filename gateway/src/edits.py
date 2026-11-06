"""Source of the "AI profiles" patch (LLM via Ollama + image checkpoints) for comfy-colab-kit.

Shared by the workbench's embedded kit (kit/gate, kit/colab) and the VPS gateway
(apps/gate, packages/colab). build.py turns this into the one-click gate-ai.py.
"""

PARSE_AI = r'''
AI_TYPES = ("llm", "image")


def _parse_ai(raw, models: dict, pid: str) -> dict | None:
    """Optional `ai:` block: what the workbench "AI" page offers once this profile is up.

    ai:
      type: llm | image
      title: shown in the workbench
      llm:   {model: <model id of a .gguf under llm/>, name: <ollama name>, ctx: 32768, think: true,
              temperature: 0.7, system: "..."}
      image: {arch: sdxl | chroma, model: <model id>, clip: <id> (chroma), vae: <id> (chroma),
              width: 1024, height: 1024, steps: 30, cfg: 5, sampler: dpmpp_sde, scheduler: karras,
              shift: 1.0 (chroma), negative: "..."}
        sdxl:   model is a checkpoint under checkpoints/
        chroma: model under diffusion_models/, clip (T5) under text_encoders/, vae under vae/
    The referenced models are downloaded as core on wake.
    """
    if not raw:
        return None
    ctx = f"{pid}.ai"
    if not isinstance(raw, dict):
        raise ProfileError(f"{ctx} must be a mapping")
    kind = raw.get("type")
    if kind not in AI_TYPES:
        raise ProfileError(f"{ctx}.type must be one of {AI_TYPES}")
    mids = [str(m) for m in (raw.get("models") or [])]
    out: dict[str, Any] = {"type": kind, "title": str(raw.get("title") or "")[:80]}
    if kind == "llm":
        llm = raw.get("llm") or {}
        mid = str(_req(llm, "model", f"{ctx}.llm"))
        if mid not in models or not models[mid].dest.startswith("llm/") or not models[mid].dest.endswith(".gguf"):
            raise ProfileError(f"{ctx}.llm.model must be a model id whose dest is llm/<file>.gguf")
        name = str(llm.get("name") or "wb-llm")
        if not re.fullmatch(r"[a-z0-9][a-z0-9._:-]{0,62}", name):
            raise ProfileError(f"{ctx}.llm.name must be lowercase letters, digits, . _ : -")
        try:
            ctxlen = max(2048, min(262144, int(llm.get("ctx", 32768))))
            temp = float(llm.get("temperature", 0.7))
        except (TypeError, ValueError):
            raise ProfileError(f"{ctx}.llm: ctx must be an integer and temperature a number") from None
        out["llm"] = {"model": mid, "dest": models[mid].dest, "name": name, "ctx": ctxlen,
                      "think": bool(llm.get("think", True)), "temperature": temp,
                      "system": str(llm.get("system") or "")[:4000]}
        uses = [mid]
    else:
        img = raw.get("image") or {}
        arch = str(img.get("arch") or "sdxl")
        if arch not in ("sdxl", "chroma"):
            raise ProfileError(f"{ctx}.image.arch must be sdxl or chroma")

        def need(key, folders):
            m = str(_req(img, key, f"{ctx}.image"))
            if m not in models or models[m].dest.split("/", 1)[0] not in folders:
                raise ProfileError(f"{ctx}.image.{key} must be a model id under {' or '.join(folders)}/")
            return m

        base = lambda m: models[m].dest.split("/", 1)[1]  # noqa: E731
        d: dict[str, Any] = {"arch": arch}
        if arch == "sdxl":
            d["model"] = need("model", ("checkpoints",))
            d["checkpoint"] = base(d["model"])
            uses = [d["model"]]
            dflt = (1024, 1024, 30, 5.0, "dpmpp_sde", "karras")
        else:
            d["model"], d["clip"], d["vae"] = need("model", ("diffusion_models", "unet")), \
                need("clip", ("text_encoders", "clip")), need("vae", ("vae",))
            d["unet_name"], d["clip_name"], d["vae_name"] = base(d["model"]), base(d["clip"]), base(d["vae"])
            uses = [d["model"], d["clip"], d["vae"]]
            dflt = (1024, 1024, 26, 3.8, "euler", "beta")
        for k, typ, dv in (("width", int, dflt[0]), ("height", int, dflt[1]), ("steps", int, dflt[2]),
                           ("cfg", float, dflt[3]), ("sampler", str, dflt[4]), ("scheduler", str, dflt[5]),
                           ("negative", str, ""), ("shift", float, 1.0)):
            try:
                d[k] = typ(img.get(k, dv))
            except (TypeError, ValueError):
                raise ProfileError(f"{ctx}.image.{k} has the wrong type") from None
        out["image"] = d
    for m in uses:
        if m not in mids:
            mids.append(m)
    for m in mids:
        if m not in models:
            raise ProfileError(f"{ctx}.models: unknown model '{m}'")
    out["models"] = mids
    return out


def parse_profile('''

PROFILES = [
    ('"rapid_moss", "model_patches"}', '"rapid_moss", "model_patches", "llm"}'),
    ("    model_aliases: dict = field(default_factory=dict)\n",
     "    model_aliases: dict = field(default_factory=dict)\n    ai: dict | None = None  # wb-ai\n"),
    ("        ids |= set(self.smoke.models if self.smoke else ())\n",
     "        ids |= set(self.smoke.models if self.smoke else ())\n        ids |= set((self.ai or {}).get(\"models\") or ())\n"),
    ('            "app_default": self.app_default,\n',
     '            "app_default": self.app_default,\n            "ai": self.ai,\n'),
    # upgrade path: an older _parse_ai (between AI_TYPES and parse_profile) is replaced in place
    ("\ndef parse_profile(", PARSE_AI, ("\nAI_TYPES = (", "\ndef parse_profile(")),
    ("    return Profile(\n        model_aliases=",
     "    ai = _parse_ai(data.get(\"ai\"), models, pid)\n    return Profile(\n        model_aliases="),
    ("app_default=app_default, smoke=smoke, raw=data,", "app_default=app_default, smoke=smoke, raw=data, ai=ai,"),
]

PLAN = [
    ("    default_core = set(m.id for m in profile.models_for([profile.app_default])) if profile.app_default else set()\n",
     "    ai_models = [profile.models[m] for m in ((getattr(profile, \"ai\", None) or {}).get(\"models\") or [])]  # wb-ai\n"
     "    for m in ai_models:\n        models.setdefault(m.id, m)\n"
     "    default_core = set(m.id for m in profile.models_for([profile.app_default])) if profile.app_default else set()\n"
     "    default_core |= {m.id for m in ai_models}\n"),
    ('        "default_view": settings.default_view,\n',
     '        "ai": getattr(profile, "ai", None),\n        "default_view": settings.default_view,\n'),
]

AI_SETUP = r'''
# ── wb-ai: local LLM (Ollama) + the /wbllm proxy route inside ComfyUI ────────────────────────────
OLLAMA_URL = "http://127.0.0.1:11434"
AI_INFO = "/content/wb_ai.json"

AI_NODE = r"""
# wb_ai: exposes the VM's local LLM server (Ollama, 127.0.0.1:11434) on ComfyUI's port, so the gate /
# tunnel reach it at /wbllm/... with the same auth as ComfyUI. Only read/chat endpoints are passed on.
import json
import aiohttp
from aiohttp import web
from server import PromptServer

NODE_CLASS_MAPPINGS = {}
OLLAMA = "http://127.0.0.1:11434"
ALLOWED = ("api/chat", "api/generate", "api/tags", "api/ps", "api/show", "api/version",
           "v1/chat/completions", "v1/models")
_routes = PromptServer.instance.routes


@_routes.get("/wbllm/info")
async def _wb_info(request):
    try:
        info = json.load(open("/content/wb_ai.json"))
    except Exception:
        info = {}
    up = False
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=3)) as s:
            async with s.get(OLLAMA + "/api/version") as r:
                up = r.status == 200
    except Exception:
        pass
    return web.json_response({**info, "llm_up": up})


@_routes.route("*", "/wbllm/{tail:.*}")
async def _wb_proxy(request):
    tail = request.match_info["tail"]
    if tail not in ALLOWED:
        return web.json_response({"error": "not allowed"}, status=404)
    body = await request.read()
    sess = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=None, sock_connect=5, sock_read=900))
    try:
        async with sess.request(request.method, OLLAMA + "/" + tail, data=body or None,
                                headers={"Content-Type": request.headers.get("Content-Type", "application/json")}) as r:
            resp = web.StreamResponse(status=r.status, headers={
                "Content-Type": r.headers.get("Content-Type", "application/json"),
                "Cache-Control": "no-cache", "X-Accel-Buffering": "no"})
            await resp.prepare(request)
            async for chunk in r.content.iter_any():
                await resp.write(chunk)
            await resp.write_eof()
            return resp
    except aiohttp.ClientConnectorError:
        return web.json_response({"error": "LLM server is not running"}, status=503)
    finally:
        await sess.close()
"""


def _ollama_env():
    llm = (PLAN.get("ai") or {}).get("llm") or {}
    env = {**os.environ, "OLLAMA_HOST": "127.0.0.1:11434", "OLLAMA_MODELS": "/content/ollama_models",
           "OLLAMA_KEEP_ALIVE": "-1", "OLLAMA_CONTEXT_LENGTH": str(llm.get("ctx", 32768)),
           "OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_PARALLEL": "1"}
    for k in ("HF_TOKEN", "CF_TUNNEL_TOKEN"):
        env.pop(k, None)
    return env


def _ollama_up(timeout=3):
    try:
        urllib.request.urlopen(OLLAMA_URL + "/api/version", timeout=timeout)
        return True
    except Exception:
        return False


OLLAMA_URL_DL = "https://ollama.com/download/ollama-linux-amd64.tar.zst"


def _ollama_bin():
    return shutil.which("ollama") or ("/usr/local/bin/ollama" if os.path.isfile("/usr/local/bin/ollama") else "")


def ollama_install():
    """Install the Ollama release tarball into /usr/local without install.sh: no apt / dpkg (whose lock the
    concurrent cloudflared .deb install holds), no systemd. The .tar.zst is streamed through Python."""
    import tarfile
    logf = open(f"{LOG}/ollama_install.log", "a")

    def note(msg):
        logf.write(msg + "\n")
        logf.flush()

    try:
        import zstandard
    except ImportError:
        subprocess.run([sys.executable, "-m", "pip", "install", "-q", "zstandard"], stdout=logf, stderr=subprocess.STDOUT)
        import zstandard
    last = None
    for attempt in range(1, 4):
        try:
            req = urllib.request.Request(OLLAMA_URL_DL, headers={"User-Agent": "wakeloom-kit"})
            with urllib.request.urlopen(req, timeout=60) as r:
                note(f"download {r.geturl()[:120]} ({r.headers.get('Content-Length')} bytes)")
                with zstandard.ZstdDecompressor().stream_reader(r) as zr, tarfile.open(fileobj=zr, mode="r|") as tf:
                    try:
                        tf.extractall("/usr/local", filter="tar")
                    except TypeError:  # Python without extraction filters
                        tf.extractall("/usr/local")
            if os.path.isfile("/usr/local/bin/ollama"):
                os.chmod("/usr/local/bin/ollama", 0o755)
                note("installed /usr/local/bin/ollama")
                return
            last = RuntimeError("bin/ollama missing from the archive")
        except Exception as e:  # noqa: BLE001
            last = e
        note(f"attempt {attempt} failed: {last!r}")
        time.sleep(5 * attempt)
    raise RuntimeError(f"Ollama install failed: {last!r}")


def ollama_serve():
    os.makedirs(LOG, exist_ok=True)
    subprocess.Popen([_ollama_bin() or "ollama", "serve"], env=_ollama_env(), stdout=open(f"{LOG}/ollama.log", "a"),
                     stderr=subprocess.STDOUT, start_new_session=True)
    for _ in range(120):
        if _ollama_up():
            return
        time.sleep(1)
    raise RuntimeError("ollama serve did not start (see kit_logs/ollama.log)")


def ai_prepare():
    """Start installing Ollama in the background at T0 (it overlaps the model download)."""
    ai = PLAN.get("ai") or {}
    json.dump({k: v for k, v in ai.items() if k != "models"}, open(AI_INFO, "w"))
    if ai.get("type") != "llm":
        return None
    box = {}

    def run():
        try:
            if not _ollama_bin():
                ollama_install()
            ollama_serve()
        except Exception as e:  # noqa: BLE001
            box["error"] = e

    t = threading.Thread(target=run, name="wb_ollama_install", daemon=True)
    t.start()
    return t, box


def ai_finish(prep):
    """After the downloads: register the GGUF with Ollama and load it onto the GPU."""
    ai = PLAN.get("ai") or {}
    if ai.get("type") != "llm" or not prep:
        return
    t, box = prep
    llm = ai["llm"]
    with Stage("llm"):
        t.join(900)
        if t.is_alive():
            raise RuntimeError("Ollama install timed out")
        if box.get("error"):
            raise box["error"]
        path = os.path.realpath(f"{COMFY}/models/{llm['dest']}")
        if not os.path.isfile(path):
            raise RuntimeError(f"LLM model file missing: {llm['dest']}")
        mf = "/content/wb_Modelfile"
        open(mf, "w").write(f"FROM {path}\n")
        print(f"registering {os.path.basename(path)} with Ollama as {llm['name']} …", flush=True)
        sh(f"{_ollama_bin()} create {llm['name']} -f {mf} 2>&1 | tr '\\r' '\\n' | sed -e 's/\\x1b\\[[0-9;?]*[a-zA-Z]//g' | grep -v '^\\s*$' | tail -3",
           env=_ollama_env())
        print("loading the model onto the GPU …", flush=True)
        req = urllib.request.Request(OLLAMA_URL + "/api/generate", method="POST",
                                     data=json.dumps({"model": llm["name"], "keep_alive": -1}).encode(),
                                     headers={"Content-Type": "application/json"})
        urllib.request.urlopen(req, timeout=1200).read()
        sh(f"{_ollama_bin()} ps", check=False, env=_ollama_env())
        info = json.load(open(AI_INFO))
        info["ready"] = True
        json.dump(info, open(AI_INFO, "w"))


def install_custom_nodes():'''

BOOT_OLD = """        if not os.path.isdir(COMFY):
            procs.append(subprocess.Popen(
                f"git clone -q {cfg['repo']} {COMFY} && cd {COMFY} && git checkout -q {cfg['commit']}", shell=True))
"""
BOOT_NEW = """        if not os.path.isfile(f"{COMFY}/main.py"):
            # wb-boot: a model too big for the RAM store downloads straight into ComfyUI/models and can create
            # the folder before this clone runs -> clone beside it and merge, instead of skipping the clone
            procs.append(subprocess.Popen(
                f"rm -rf /content/_comfy_src && git clone -q {cfg['repo']} /content/_comfy_src && "
                f"cd /content/_comfy_src && git checkout -q {cfg['commit']} && mkdir -p {COMFY} && "
                f"cp -a /content/_comfy_src/. {COMFY}/ && rm -rf /content/_comfy_src", shell=True))
"""

SETUP = [
    (BOOT_OLD, BOOT_NEW),
    ("\ndef install_custom_nodes():", AI_SETUP, ("\n# ── wb-ai: local LLM", "\ndef install_custom_nodes():")),
    ('    open(f"{base}/kit_heartbeat/web/kit.js", "w").write(KIT_JS)\n',
     '    open(f"{base}/kit_heartbeat/web/kit.js", "w").write(KIT_JS)\n'
     '    os.makedirs(f"{base}/wb_ai", exist_ok=True)\n'
     '    open(f"{base}/wb_ai/__init__.py", "w").write(AI_NODE)\n'),
    ('    with Stage("bootstrap"):\n', '    ai_prep = ai_prepare()\n    with Stage("bootstrap"):\n'),
    ('    if not any(t.name == "kit_watchdog" for t in threading.enumerate()):\n',
     '    ai_finish(ai_prep)\n    if not any(t.name == "kit_watchdog" for t in threading.enumerate()):\n'),
    ('        if time.time() - t0 > max_s:\n',
     '        if (PLAN.get("ai") or {}).get("type") == "llm" and _ollama_bin() and not _ollama_up():\n'
     '            print("[watchdog] ollama died; restarting", flush=True)\n'
     '            try:\n                ollama_serve()\n            except Exception as e:  # noqa: BLE001\n'
     '                print(f"[watchdog] ollama restart failed: {e!r}", flush=True)\n'
     '        if time.time() - t0 > max_s:\n',
     ('        if (PLAN.get("ai") or {}).get("type") == "llm"', '        if time.time() - t0 > max_s:\n')),
]

MARKS = {"profiles": "def _parse_ai(", "plan": "# wb-ai", "setup": "def ai_prepare("}

PROFILE_QWEN = """\
# Huihui Qwen3.8 27B abliterated (GGUF Q8_0, 29 GB) served by Ollama for the workbench "AI" page.
# Needs >= 40 GB VRAM: G4 / A100 / H100 (L4 and T4 are too small).
id: qwen38-27b-chat
title: Qwen3.8 27B abliterated · Q8_0 对话
accelerator: G4
comfyui:
  repo: https://github.com/comfyanonymous/ComfyUI
  commit: 2c75dc1b
  args: [--preview-method, auto]
frontend:
  package: comfyui-frontend-package==1.57.6
models:
- id: qwen38-27b-q8
  dest: llm/Huihui-Qwen3.8-27B-abliterated-Q8_0.gguf
  url: hf://huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF/Huihui-Qwen3.8-27B-abliterated-Q8_0.gguf
  license: apache-2.0
  size_gb: 29.05
ai:
  type: llm
  title: Qwen3.8 27B abliterated
  llm:
    model: qwen38-27b-q8
    name: qwen3.8-27b
    ctx: 32768
    think: false
    temperature: 0.7
"""

PROFILE_REALVIS = """\
# RealVisXL V5.0 (SDXL, photorealism) for the workbench "AI" page text-to-image panel. ~7 GB, any GPU.
id: realvisxl-v5
title: RealVisXL V5.0 · 写实生图
accelerator: L4
comfyui:
  repo: https://github.com/comfyanonymous/ComfyUI
  commit: 2c75dc1b
  args: [--preview-method, auto]
frontend:
  package: comfyui-frontend-package==1.57.6
models:
- id: realvisxl-v5
  dest: checkpoints/RealVisXL_V5.0_fp16.safetensors
  url: hf://SG161222/RealVisXL_V5.0/RealVisXL_V5.0_fp16.safetensors
  license: openrail++
  size_gb: 6.94
ai:
  type: image
  title: RealVisXL V5.0
  image:
    model: realvisxl-v5
    width: 832
    height: 1216
    steps: 30
    cfg: 5
    sampler: dpmpp_sde
    scheduler: karras
    negative: "bad hands, bad anatomy, ugly, deformed, (face asymmetry, eyes asymmetry, deformed eyes, deformed mouth, open mouth)"
"""

PROFILE_CHROMA = """\
# Chroma1-HD (8.9B, FLUX.1-schnell based, Apache 2.0) for the workbench "AI" page text-to-image panel.
# bf16 model 17.8 GB + T5-XXL fp16 9.8 GB + FLUX VAE; same graph as the official ComfyUI template.
id: chroma1-hd
title: Chroma1-HD · 生图
accelerator: G4
comfyui:
  repo: https://github.com/comfyanonymous/ComfyUI
  commit: 2c75dc1b
  args: [--preview-method, auto]
frontend:
  package: comfyui-frontend-package==1.57.6
models:
- id: chroma1-hd
  dest: diffusion_models/Chroma1-HD.safetensors
  url: hf://lodestones/Chroma1-HD/Chroma1-HD.safetensors
  license: apache-2.0
  size_gb: 17.8
- id: t5xxl-fp16
  dest: text_encoders/t5xxl_fp16.safetensors
  url: hf://comfyanonymous/flux_text_encoders/t5xxl_fp16.safetensors
  license: apache-2.0
  size_gb: 9.79
- id: flux-ae
  dest: vae/ae.safetensors
  url: hf://Comfy-Org/Lumina_Image_2.0_Repackaged/split_files/vae/ae.safetensors
  license: apache-2.0
  size_gb: 0.34
ai:
  type: image
  title: Chroma1-HD
  image:
    arch: chroma
    model: chroma1-hd
    clip: t5xxl-fp16
    vae: flux-ae
    width: 1024
    height: 1024
    steps: 26
    cfg: 3.8
    sampler: euler
    scheduler: beta
    shift: 1.0
    negative: "This greyscale unfinished sketch has bad proportions, is featureless and disfigured. It is a blurry ugly mess and with excessive gaussian blur. It is riddled with watermarks and signatures. Everything is smudged with leaking colors and nonsensical orientation of objects. Messy and abstract image filled with artifacts disrupt the coherency of the overall composition. The image has extreme chromatic abberations and inconsistent lighting. Dull, monochrome colors and countless artistic errors."
"""
