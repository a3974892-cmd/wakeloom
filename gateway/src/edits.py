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
        projector = str(llm.get("projector") or "")
        if projector:
            if (projector == mid or projector not in models or
                    not models[projector].dest.startswith("llm/") or not models[projector].dest.endswith(".gguf")):
                raise ProfileError(f"{ctx}.llm.projector must be a separate model id under llm/<file>.gguf")
            out["llm"]["projector"] = projector
            out["llm"]["projector_dest"] = models[projector].dest
            uses.append(projector)
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

MODEL_DIRS = '''MODEL_DIRS = {"checkpoints", "diffusion_models", "text_encoders", "clip", "vae", "loras", "upscale_models",
              "controlnet", "embeddings", "clip_vision", "unet", "rapid_moss", "model_patches", "llm",
              # wb-dirs: more ComfyUI model folders for user-added profiles
              "style_models", "gligen", "hypernetworks", "photomaker", "vae_approx", "audio_encoders",
              "latent_upscale_models", "ipadapter", "sams", "ultralytics", "insightface", "instantid"}
'''

CUSTOM = r'''
# ── wb-custom: user-added profiles (Wakeloom "添加模型"), STATE_DIR/custom_profiles/<id>/profile.yaml ──
CUSTOM_DIR = "custom_profiles"
CUSTOM_PREFIX = "custom-"
CUSTOM_MAX_BYTES = 64 * 1024


def custom_root(state_dir) -> Path:
    return Path(state_dir) / CUSTOM_DIR


def custom_ids(state_dir) -> set[str]:
    root = custom_root(state_dir)
    if not root.is_dir():
        return set()
    return {d.name for d in root.iterdir() if (d / "profile.yaml").is_file()}


def load_custom(state_dir, builtin=()) -> tuple[dict[str, Profile], dict[str, str]]:
    """Profiles saved through /_gate/custom-profiles. They never shadow a built-in (PROFILES) id."""
    ok: dict[str, Profile] = {}
    errors: dict[str, str] = {}
    for name in sorted(custom_ids(state_dir)):
        try:
            prof = load_profile(custom_root(state_dir) / name / "profile.yaml")
            if prof.id != name:
                raise ProfileError(f"id {prof.id!r} does not match its folder {name!r}")
            if prof.id in builtin:
                raise ProfileError(f"{prof.id!r} would replace a built-in profile")
            ok[prof.id] = prof
        except (ProfileError, yaml.YAMLError, OSError) as e:
            errors[f"custom:{name}"] = str(e)
    return ok, errors


def save_custom(state_dir, text, builtin=()) -> Profile:
    """Validate a user-added profile.yaml (same rules as every profile) and store it."""
    if not isinstance(text, str) or not text.strip():
        raise ProfileError("profile.yaml is empty")
    if len(text.encode("utf-8")) > CUSTOM_MAX_BYTES:
        raise ProfileError(f"profile.yaml is larger than {CUSTOM_MAX_BYTES // 1024} KB")
    try:
        data = yaml.safe_load(text)
    except yaml.YAMLError as e:
        raise ProfileError(f"invalid YAML: {e}") from None
    if not isinstance(data, dict):
        raise ProfileError("profile must be a mapping")
    pid = str(data.get("id") or "")
    if not pid.startswith(CUSTOM_PREFIX):
        raise ProfileError(f"custom profile ids must start with {CUSTOM_PREFIX!r}")
    if pid in builtin:
        raise ProfileError(f"{pid!r} is a built-in profile and cannot be replaced")
    prof = parse_profile(data, custom_root(state_dir) / pid)  # also rejects ids that are not [a-z0-9-]
    problem = custom_problem(prof)
    if problem:
        raise ProfileError(problem)
    d = custom_root(state_dir) / prof.id
    d.mkdir(parents=True, exist_ok=True)
    tmp = d / "profile.yaml.tmp"
    tmp.write_text(text, encoding="utf-8")
    tmp.replace(d / "profile.yaml")
    return prof


CUSTOM_MAX_GB = 96 * 0.9  # biggest Colab GPU (G4, 96 GB) with headroom


def custom_problem(prof: Profile) -> str:
    """Why a user-added profile could not be used ('' = fine): same checks as Wakeloom's customProblem()."""
    used = {mid for w in prof.workflows.values() for mid in w.models}
    used |= set(prof.smoke.models if prof.smoke else ()) | set((prof.ai or {}).get("models") or ())
    used |= {m.id for m in prof.models.values() if m.core}
    if not used:
        return "no model would be downloaded: mark the files core: true or reference them from the ai block"
    entry = prof.raw.get("wb_entry") or {}
    if not prof.ai and not (isinstance(entry, dict) and str(entry.get("template") or "").strip()):
        return "nothing to use it with: needs an ai block (llm / image) or a linked template (wb_entry.template)"
    total = sum(prof.models[m].size_gb or 0 for m in used)
    if total > CUSTOM_MAX_GB:
        return f"the models need about {total:.0f} GB, more than the {CUSTOM_MAX_GB:.1f} GB a Colab GPU can hold"
    return ""


def delete_custom(state_dir, pid) -> bool:
    import shutil
    if not ID_RE.match(str(pid or "")) or pid not in custom_ids(state_dir):
        return False
    shutil.rmtree(custom_root(state_dir) / pid)
    return True


def hf_to_https('''

PROFILES = [
    ('"rapid_moss", "model_patches"}\n', MODEL_DIRS, ("MODEL_DIRS = {", "}\n")),
    ("\ndef hf_to_https(", CUSTOM, ("\n# ── wb-custom: user-added profiles", "\ndef hf_to_https(")),
    ("    model_aliases: dict = field(default_factory=dict)\n",
     "    model_aliases: dict = field(default_factory=dict)\n    ai: dict | None = None  # wb-ai\n"),
    ("        ids |= set(self.smoke.models if self.smoke else ())\n",
     "        ids |= set(self.smoke.models if self.smoke else ())\n        ids |= set((self.ai or {}).get(\"models\") or ())\n"),
    ("        ids |= set((self.ai or {}).get(\"models\") or ())\n",
     "        ids |= set((self.ai or {}).get(\"models\") or ())\n"
     "        used = {mid for w in self.workflows.values() for mid in w.models} | ids  # wb-core: see plan.py\n"
     "        ids |= {m.id for m in self.models.values() if m.core and m.id not in used}\n"),
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
    ("    default_core |= {m.id for m in ai_models}\n",
     "    default_core |= {m.id for m in ai_models}\n"
     "    # wb-core: `core: true` models that no workflow / smoke test / ai block uses (user-added \"any model\"\n"
     "    # profiles) are downloaded on every wake too; models a workflow uses keep that workflow's rules\n"
     "    used = {mid for w in profile.workflows.values() for mid in w.models}\n"
     "    used |= set(profile.smoke.models if profile.smoke else ()) | {m.id for m in ai_models}\n"
     "    for m in profile.models.values():\n"
     "        if m.core and m.id not in used:\n"
     "            models.setdefault(m.id, m)\n"),
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


__WB_OLLAMA_INSTALL__

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


def _dir_bytes(d):
    total = 0
    for root, _, files in os.walk(d):
        for f in files:
            try:
                total += os.path.getsize(os.path.join(root, f))
            except OSError:
                pass
    return total


def _ollama_seed_blob(path, every=10):
    """Reuse the completed GGUF on the same filesystem instead of copying it into Ollama.
    The normal CLI still hashes the source, registers the model and chooses its original defaults.
    If linking is unavailable, leave both files untouched and use the original import path."""
    import hashlib

    blobs = os.path.join(_ollama_env()["OLLAMA_MODELS"], "blobs")
    linked = None
    try:
        os.makedirs(blobs, exist_ok=True)
        source = os.stat(path)
        if source.st_size <= 0 or source.st_dev != os.stat(blobs).st_dev:
            print("  blob reuse unavailable across filesystems; using standard import", flush=True)
            return False

        def identity(st):
            return st.st_dev, st.st_ino, st.st_size, st.st_mtime_ns

        digest = hashlib.sha256()
        done, last = 0, time.monotonic()
        print("  preparing model reuse (SHA-256 check) …", flush=True)
        with open(path, "rb") as f:
            if identity(os.fstat(f.fileno())) != identity(source):
                return False
            for block in iter(lambda: f.read(8 << 20), b""):
                digest.update(block)
                done += len(block)
                if time.monotonic() - last >= every:
                    print(f"  checked {done / 1e9:.1f}/{source.st_size / 1e9:.1f} GB", flush=True)
                    last = time.monotonic()
            if identity(os.fstat(f.fileno())) != identity(source):
                return False
        if identity(os.stat(path)) != identity(source):
            return False  # source changed; the original CLI will check it again
        blob = os.path.join(blobs, "sha256-" + digest.hexdigest())
        if os.path.lexists(blob):
            return False  # never replace a pre-existing blob or link
        os.link(path, blob)
        linked = blob
        if identity(os.stat(blob)) != identity(source):
            os.unlink(blob)
            return False
        print(f"  reusing {source.st_size / 1e9:.1f} GB model without an extra copy", flush=True)
        return True
    except OSError as e:
        if linked:
            try:
                os.unlink(linked)
            except OSError:
                pass
        print(f"  blob reuse unavailable ({type(e).__name__}); using standard import", flush=True)
        return False


def _ollama_create(name, mf, path, every=10):
    """Without a reusable blob, `ollama create` copies the GGUF into OLLAMA_MODELS; its progress bar is only
    carriage-return redraws, so the log looked frozen for minutes. Read its output ourselves and print a progress line
    every `every` seconds: elapsed time, the latest status from ollama, and how much has been written."""
    size = os.path.getsize(path)
    blobs = os.path.join(_ollama_env()["OLLAMA_MODELS"], "blobs")
    print(f"registering {os.path.basename(path)} ({size / 1e9:.1f} GB) with Ollama as {name} …", flush=True)
    t0 = time.time()
    reused = _ollama_seed_blob(path, every)
    base = _dir_bytes(blobs)
    p = subprocess.Popen([_ollama_bin() or "ollama", "create", name, "-f", mf], env=_ollama_env(),
                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    state = {"last": "", "tail": []}
    ansi = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]")

    def reader():
        buf = b""
        while True:
            ch = p.stdout.read(1)
            if not ch:
                break
            if ch in (b"\r", b"\n"):
                line = ansi.sub("", buf.decode("utf-8", "replace")).strip()
                buf = b""
                if line:
                    state["last"] = line
                    if not state["tail"] or state["tail"][-1] != line:
                        state["tail"] = (state["tail"] + [line])[-5:]
            else:
                buf += ch
        p.stdout.close()

    t = threading.Thread(target=reader, name="wb_ollama_create", daemon=True)
    t.start()
    while True:
        try:
            rc = p.wait(timeout=every)
            break
        except subprocess.TimeoutExpired:
            done = max(0, _dir_bytes(blobs) - base)
            pct = f" ({min(100, done * 100 // size)}%)" if size else ""
            msg = (f"  … {int(time.time() - t0)}s, reusing model file; preparing Ollama metadata" if reused else
                   f"  … {int(time.time() - t0)}s, written {done / 1e9:.1f}/{size / 1e9:.1f} GB{pct}")
            if state["last"]:
                msg += f" · {state['last'][:100]}"
            print(msg, flush=True)
    t.join(5)
    for line in state["tail"][-3:]:
        print(f"  {line[:200]}", flush=True)
    if rc:
        raise RuntimeError(f"ollama create failed ({rc}): {state['last']}")
    print(f"registered {name} in {int(time.time() - t0)}s", flush=True)


def ai_finish(prep):
    """After the downloads: register the GGUF with Ollama and load it onto the GPU."""
    ai = PLAN.get("ai") or {}
    if ai.get("type") == "image":
        with Stage("image_models"):
            wanted = set(ai.get("models") or [])
            for m in PLAN["models"]:
                if m["id"] not in wanted:
                    continue
                path = f"{COMFY}/models/{m['dest']}"
                if not os.path.isfile(path) or os.path.getsize(path) == 0:
                    raise RuntimeError(f"Image model file missing or empty after download: {m['dest']}")
                print(f"verified image model: {m['dest']}", flush=True)
            im = ai["image"]
            loaders = [("CheckpointLoaderSimple", "ckpt_name", im.get("checkpoint"))]
            if im.get("arch") == "chroma":
                loaders = [("UNETLoader", "unet_name", im.get("unet_name")),
                           ("CLIPLoader", "clip_name", im.get("clip_name")),
                           ("VAELoader", "vae_name", im.get("vae_name"))]
            for cls, field, name in loaders:
                url = f"http://127.0.0.1:{PORT}/object_info/{cls}"
                with urllib.request.urlopen(url, timeout=20) as r:
                    node = json.load(r)
                names = node.get(cls, {}).get("input", {}).get("required", {}).get(field, [[]])[0]
                if not name or name not in names:
                    raise RuntimeError(f"ComfyUI cannot find image model {name} in {cls} ({len(names)} available)")
            with open(AI_INFO) as f:
                info = json.load(f)
            info["ready"] = True
            with open(AI_INFO, "w") as f:
                json.dump(info, f)
        return
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
        modelfile = f"FROM {path}\n"
        if llm.get("projector_dest"):
            projector = os.path.realpath(f"{COMFY}/models/{llm['projector_dest']}")
            if not os.path.isfile(projector) or os.path.getsize(projector) == 0:
                raise RuntimeError(f"Vision projector file missing or empty: {llm['projector_dest']}")
            modelfile += f'FROM "{projector}"\n'
            _ollama_seed_blob(projector)
        with open(mf, "w") as f:
            f.write(modelfile)
        _ollama_create(llm["name"], mf, path)
        if llm.get("projector_dest"):
            req = urllib.request.Request(OLLAMA_URL + "/api/show", method="POST",
                                         data=json.dumps({"model": llm["name"]}).encode(),
                                         headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=60) as response:
                capabilities = json.load(response).get("capabilities") or []
            if "vision" not in capabilities:
                raise RuntimeError("Ollama did not enable vision for this model. Update Ollama and check projector compatibility.")
            print("vision capability verified", flush=True)
        print("loading the model onto the GPU …", flush=True)
        req = urllib.request.Request(OLLAMA_URL + "/api/generate", method="POST",
                                     data=json.dumps({"model": llm["name"], "keep_alive": -1}).encode(),
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=1200) as response:
            response.read()
        sh(f"{_ollama_bin()} ps", check=False, env=_ollama_env())
        with open(AI_INFO) as f:
            info = json.load(f)
        info["ready"] = True
        with open(AI_INFO, "w") as f:
            json.dump(info, f)


def install_custom_nodes():'''

from ollama_installer import INSTALL_SOURCE
AI_SETUP = AI_SETUP.replace("__WB_OLLAMA_INSTALL__", INSTALL_SOURCE)

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

BOOTSTRAP_LEGACY = '''    with Stage("bootstrap"):
        # uv is only needed by the deps stage -> install it in the background while cloudflared
        # and the ComfyUI clone run in parallel.
        pip_uv = subprocess.Popen([sys.executable, "-m", "pip", "install", "-q", "-U", "uv"],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT)
        procs = []
        if not shutil.which("cloudflared"):
            procs.append(subprocess.Popen(
                "wget -q -O /tmp/cf.deb https://github.com/cloudflare/cloudflared/releases/latest/download/"
                "cloudflared-linux-amd64.deb && dpkg -i /tmp/cf.deb >/dev/null", shell=True))
''' + BOOT_NEW + '''        for p in procs:
            if p.wait() != 0:
                raise RuntimeError("bootstrap step failed")
        dl.sync_links()
        start_tunnel()  # cloudflared retries until :8188 answers, so start it early
        sh("nvidia-smi --query-gpu=name,memory.total --format=csv,noheader", check=False)
        pip_uv.wait()  # uv must be on PATH before the deps stage uses it
'''

BOOTSTRAP_HELPERS = r'''
# ── wb-bootstrap: named, bounded bootstrap jobs with captured child-process output ──
def _wb_bootstrap_tail(logfile):
    with open(logfile, "rb") as f:
        f.seek(0, 2)
        f.seek(max(0, f.tell() - 8192))
        text = f.read().decode("utf-8", "replace")
    for key in ("HF_TOKEN", "CF_TUNNEL_TOKEN"):
        value = os.environ.get(key)
        if value:
            text = text.replace(value, "[redacted]")
    text = re.sub(r"(?i)(Bearer\s+)\S+", r"\1[redacted]", text)
    text = re.sub(r"(https?://)[^\s/@]+:[^\s/@]+@", r"\1[redacted]@", text)
    return "\n".join(text.splitlines()[-20:])


def _wb_bootstrap_command(label, argv, cancel, attempts=1, timeout=300):
    os.makedirs(LOG, exist_ok=True)
    last = ""
    for attempt in range(1, attempts + 1):
        if cancel.is_set():
            raise RuntimeError(f"{label}: canceled after another bootstrap failure")
        logfile = os.path.join(LOG, f"bootstrap-{label}-{attempt}.log")
        print(f"KIT_BOOTSTRAP {label} attempt {attempt}/{attempts}", flush=True)
        started, next_note = time.monotonic(), time.monotonic() + 10
        with open(logfile, "wb") as output:
            p = subprocess.Popen(argv, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
            reason = ""
            while p.poll() is None:
                elapsed = time.monotonic() - started
                if cancel.is_set() or elapsed >= timeout:
                    reason = "canceled" if cancel.is_set() else f"timeout after {timeout}s"
                    try:
                        if os.name == "posix":
                            os.killpg(p.pid, signal.SIGKILL)
                        else:
                            p.kill()
                    except ProcessLookupError:
                        pass
                    p.wait()
                    break
                try:
                    p.wait(timeout=min(1, max(0.01, timeout - elapsed)))
                except subprocess.TimeoutExpired:
                    pass
                if time.monotonic() >= next_note:
                    print(f"KIT_BOOTSTRAP {label} waiting {int(elapsed)}s", flush=True)
                    next_note = time.monotonic() + 10
        if p.returncode == 0 and not reason:
            print(f"KIT_BOOTSTRAP {label} done", flush=True)
            return
        tail = _wb_bootstrap_tail(logfile)
        last = f"{label}: {reason or 'exit ' + str(p.returncode)}"
        print(f"KIT_BOOTSTRAP {last}; log={logfile}", flush=True)
        for line in tail.splitlines():
            print(f"  [{label}] {line}", flush=True)
        if cancel.is_set():
            raise RuntimeError(last)
        if attempt < attempts:
            print(f"KIT_BOOTSTRAP {label} retrying", flush=True)
            cancel.wait(3)
    detail = tail.splitlines()[-1] if tail else "no child-process output"
    raise RuntimeError(f"bootstrap {last}: {detail}"[:700])


def _wb_bootstrap_download(urls, package, cancel, expected_size, expected_sha):
    import hashlib
    import urllib.request
    import urllib.error
    deadline = time.monotonic() + 180
    last = "no download route"
    for attempt in range(1, 3):
        for route, url in enumerate(urls, 1):
            if cancel.is_set():
                raise RuntimeError("cloudflared-download: canceled")
            if time.monotonic() >= deadline:
                raise RuntimeError(f"cloudflared-download: total timeout; {last}")
            print(f"KIT_BOOTSTRAP cloudflared-download route {route} attempt {attempt}/2", flush=True)
            verified = False
            try:
                req = urllib.request.Request(url, headers={"User-Agent": "wakeloom-kit",
                    "Accept": "application/octet-stream", "Cache-Control": "no-cache"})
                digest, size = hashlib.sha256(), 0
                route_deadline = min(deadline, time.monotonic() + 90)
                next_note = time.monotonic() + 5
                with urllib.request.urlopen(req, timeout=15) as response, open(package, "wb") as output:
                    if response.status != 200:
                        raise RuntimeError(f"unexpected HTTP {response.status}")
                    while True:
                        if cancel.is_set():
                            raise RuntimeError("canceled")
                        if time.monotonic() >= route_deadline:
                            raise RuntimeError("download timeout")
                        chunk = response.read(1024 * 1024)
                        if not chunk:
                            break
                        size += len(chunk)
                        if size > expected_size:
                            raise RuntimeError("package exceeds expected size")
                        output.write(chunk)
                        digest.update(chunk)
                        if time.monotonic() >= next_note:
                            print(f"KIT_BOOTSTRAP cloudflared-download {size}/{expected_size} bytes", flush=True)
                            next_note = time.monotonic() + 5
                if size != expected_size or digest.hexdigest() != expected_sha:
                    raise RuntimeError("package size or SHA-256 mismatch")
                verified = True
                print("KIT_BOOTSTRAP cloudflared-download verified", flush=True)
                return
            except Exception as exc:
                # URLs may redirect to signed assets; never include them in public logs.
                if isinstance(exc, urllib.error.HTTPError):
                    last = f"HTTP {exc.code}"
                elif isinstance(exc, RuntimeError):
                    last = str(exc)
                else:
                    last = type(exc).__name__
                print(f"KIT_BOOTSTRAP cloudflared-download route {route}: {last}", flush=True)
                if cancel.is_set():
                    raise RuntimeError("cloudflared-download: canceled") from None
            finally:
                if not verified and os.path.exists(package):
                    os.unlink(package)
        if attempt < 2:
            cancel.wait(2)
    raise RuntimeError(f"cloudflared-download: official routes failed; {last}")


def _wb_bootstrap_cloudflared(cancel):
    if shutil.which("cloudflared"):
        print("KIT_BOOTSTRAP cloudflared already installed", flush=True)
        return
    import tempfile
    with tempfile.TemporaryDirectory(prefix="wb-cloudflared-") as folder:
        package = os.path.join(folder, "cloudflared.deb")
        # Pinned official 2026.10.7 asset: API route avoids the failing browser-download path.
        _wb_bootstrap_download([
            "https://api.github.com/repos/cloudflare/cloudflared/releases/assets/692817258",
            "https://github.com/cloudflare/cloudflared/releases/download/2026.10.7/cloudflared-linux-amd64.deb",
        ], package, cancel, 19925068, "70b1a1e0f46033ffc8d90b406e554044ba6cbf2e4360295ef1163aed31e09393")
        _wb_bootstrap_command("cloudflared-install", ["dpkg", "-i", package], cancel, timeout=180)
    if not shutil.which("cloudflared"):
        raise RuntimeError("bootstrap cloudflared-install: executable missing after dpkg")


def _wb_bootstrap_comfy(cfg, cancel):
    if os.path.isfile(os.path.join(COMFY, "main.py")):
        print("KIT_BOOTSTRAP ComfyUI already installed", flush=True)
        return
    import tempfile
    # Clone beside ongoing downloads. Only owned temporary checkouts are removed on retries.
    for attempt in range(1, 4):
        with tempfile.TemporaryDirectory(prefix="wb-comfy-") as folder:
            checkout = os.path.join(folder, "source")
            try:
                _wb_bootstrap_command(f"comfyui-clone-{attempt}", ["git", "clone", "-q", cfg["repo"], checkout], cancel)
            except RuntimeError:
                if attempt == 3 or cancel.is_set():
                    raise
                print("KIT_BOOTSTRAP comfyui-clone retrying with a clean temporary checkout", flush=True)
                cancel.wait(3)
                continue
            _wb_bootstrap_command("comfyui-checkout", ["git", "-C", checkout, "checkout", "-q", cfg["commit"]], cancel)
            os.makedirs(COMFY, exist_ok=True)
            # Match the original merge operation: preserve downloads and existing model links.
            _wb_bootstrap_command("comfyui-copy", ["cp", "-a", checkout + "/.", COMFY + "/"], cancel, timeout=180)
            if not os.path.isfile(os.path.join(COMFY, "main.py")):
                raise RuntimeError("bootstrap comfyui-copy: main.py missing after checkout merge")
            return


def _wb_bootstrap_uv(cancel):
    try:
        _wb_bootstrap_command("uv-install", [sys.executable, "-m", "pip", "install", "-q", "-U", "uv"],
                              cancel, attempts=2, timeout=180)
    except Exception:
        if cancel.is_set():
            raise
        # Preserve the existing deps stage's pip fallback when uv is unavailable.
        print("KIT_BOOTSTRAP uv unavailable; dependencies will use the existing pip fallback", flush=True)


def _wb_bootstrap(cfg):
    from concurrent.futures import ThreadPoolExecutor, as_completed
    cancel = threading.Event()
    with ThreadPoolExecutor(max_workers=3, thread_name_prefix="wb-bootstrap") as pool:
        jobs = [pool.submit(_wb_bootstrap_cloudflared, cancel), pool.submit(_wb_bootstrap_comfy, cfg, cancel),
                pool.submit(_wb_bootstrap_uv, cancel)]
        try:
            for job in as_completed(jobs):
                job.result()
        except Exception:
            cancel.set()
            raise
# ── wb-bootstrap end ──


def main():'''

BOOTSTRAP_MAIN = '''    with Stage("bootstrap"):
        _wb_bootstrap(cfg)
        dl.sync_links()
        start_tunnel()
        sh("nvidia-smi --query-gpu=name,memory.total --format=csv,noheader", check=False)

    with Stage("deps"):
'''

SETUP = [
    ('        return fetch_only()\n    cfg = PLAN["comfyui"]\n',
     '        return fetch_only()\n'
     '    if not PLAN.get("models"):  # wb-empty: nothing to download usually means a broken profile\n'
     '        print("KIT_WARN no_models: this start profile has no models to download - check the profile", flush=True)\n'
     '    cfg = PLAN["comfyui"]\n'),
    ("\ndef install_custom_nodes():", AI_SETUP, ("\n# ── wb-ai: local LLM", "\ndef install_custom_nodes():")),
    ('    open(f"{base}/kit_heartbeat/web/kit.js", "w").write(KIT_JS)\n',
     '    open(f"{base}/kit_heartbeat/web/kit.js", "w").write(KIT_JS)\n'
     '    os.makedirs(f"{base}/wb_ai", exist_ok=True)\n'
     '    open(f"{base}/wb_ai/__init__.py", "w").write(AI_NODE)\n'),
    ('    with Stage("bootstrap"):\n', '    ai_prep = ai_prepare()\n    with Stage("bootstrap"):\n'),
    ('\ndef main():', '# wb-original-bootstrap: preserve the pre-deployment startup flow\n\ndef main():', ('# ── wb-bootstrap:', '\ndef main():')),
    ('', BOOTSTRAP_LEGACY + '\n    with Stage("deps"):\n', ('    with Stage("bootstrap"):\n', '\n    with Stage("deps"):\n')),
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

CUSTOM_HANDLERS = r'''
# ── wb-custom: user-added profiles (Wakeloom "添加模型") ─────────────────────────────────────────────
def _custom_denied(request: web.Request) -> web.Response | None:
    ident = _ident(request)
    if ident.is_owner or ident.kind == "agent":
        return None
    return err(403, "owner_only", "custom profiles are restricted to owners and agent tokens")


async def custom_profiles_list(request: web.Request) -> web.Response:
    """GET /_gate/custom-profiles: user-added profiles (raw profile.yaml) + load errors."""
    if (denied := _custom_denied(request)) is not None:
        return denied
    sd = request.app[SETTINGS].state_dir
    profs = request.app[SESSION].profiles
    _, errors = load_custom(sd)
    items = []
    for pid in sorted(custom_ids(sd)):
        p = profs.get(pid)
        items.append({"id": pid, "title": p.title if p else pid, "loaded": p is not None,
                      "error": errors.get(f"custom:{pid}", ""), "problem": custom_problem(p) if p else "",
                      "yaml": (custom_root(sd) / pid / "profile.yaml").read_text(encoding="utf-8")})
    return web.json_response({"items": items, "max_bytes": CUSTOM_MAX_BYTES})


async def custom_profiles_save(request: web.Request) -> web.Response:
    """POST /_gate/custom-profiles {"yaml": "<profile.yaml>"}: validate (parse_profile) and add / replace."""
    if (denied := _custom_denied(request)) is not None:
        return denied
    if (request.content_length or 0) > 2 * CUSTOM_MAX_BYTES:
        return err(413, "too_large", f"profile.yaml is larger than {CUSTOM_MAX_BYTES // 1024} KB")
    body = await _body(request)
    s = request.app[SESSION]
    sd = request.app[SETTINGS].state_dir
    builtin = set(s.profiles) - custom_ids(sd)
    try:
        prof = save_custom(sd, body.get("yaml"), builtin)
    except ProfileError as e:
        return err(400, "bad_profile", str(e))
    s.profiles[prof.id] = prof
    return web.json_response({"id": prof.id, "profile": prof.summary()})


async def custom_profiles_delete(request: web.Request) -> web.Response:
    """DELETE /_gate/custom-profiles/{id}: 409 while that profile is awake or waking."""
    if (denied := _custom_denied(request)) is not None:
        return denied
    s = request.app[SESSION]
    sd = request.app[SETTINGS].state_dir
    pid = request.match_info["id"]
    if pid not in custom_ids(sd):
        return err(404, "unknown_profile", "no such custom profile (built-in profiles cannot be deleted)")
    if s.machine.profile == pid and s.machine.state not in (State.SLEEPING, State.FAILED):
        return err(409, "in_use", "this profile is in use; stop the GPU first")
    delete_custom(sd, pid)
    s.profiles.pop(pid, None)
    return web.json_response({"deleted": pid})


async def switch(request: web.Request) -> web.Response:
'''

APP = [
    ("from .profiles import Profile, load_all\n",
     "from .profiles import Profile, load_all\n"
     "from .profiles import (CUSTOM_MAX_BYTES, ProfileError, custom_ids, custom_problem, custom_root,  # wb-custom\n"
     "                       delete_custom, load_custom, save_custom)\n",
     ("from .profiles import (CUSTOM_MAX_BYTES", "save_custom)\n")),
    ("        profiles, errors = load_all(settings.profiles, REPO_ROOT)\n    else:\n",
     "        profiles, errors = load_all(settings.profiles, REPO_ROOT)\n"
     "        custom, cerrors = load_custom(settings.state_dir, profiles)  # wb-custom\n"
     "        profiles.update(custom)\n        errors.update(cerrors)\n    else:\n"),
    ("\nasync def switch(request: web.Request) -> web.Response:\n", CUSTOM_HANDLERS,
     ("\n# ── wb-custom: user-added profiles", "\nasync def switch(request: web.Request) -> web.Response:\n")),
    ('    r.add_post("/_gate/fetch", fetch)\n',
     '    r.add_post("/_gate/fetch", fetch)\n'
     '    r.add_get("/_gate/custom-profiles", custom_profiles_list)\n'
     '    r.add_post("/_gate/custom-profiles", custom_profiles_save)\n'
     '    r.add_delete("/_gate/custom-profiles/{id}", custom_profiles_delete)\n'),
]

SESSION = [
    ("        fresh, errors = load_all(self.settings.profiles, REPO_ROOT)\n",
     "        fresh, errors = load_all(self.settings.profiles, REPO_ROOT)\n"
     "        from .profiles import load_custom  # wb-custom: user-added profiles live in STATE_DIR\n"
     "        custom, cerrors = load_custom(self.settings.state_dir, fresh)\n"
     "        fresh.update(custom)\n        errors.update(cerrors)\n"),
]

TEST_CUSTOM = r'''"""wb-custom: /_gate/custom-profiles (written by gate-ai.py)."""
from gate.profiles import load_custom

GOOD = """id: custom-tiny-llm
title: Tiny LLM
accelerator: L4
comfyui: {repo: https://github.com/comfyanonymous/ComfyUI, commit: 2c75dc1b}
models:
- {id: m, dest: llm/tiny.gguf, url: hf://o/r/tiny.gguf, license: mit, size_gb: 1.0}
ai: {type: llm, title: Tiny, llm: {model: m, name: custom-tiny-llm, ctx: 8192}}
"""


async def test_custom_profiles_crud(tmp_path):
    from tests.test_app import client
    from gate.app import SESSION

    c, app = await client(tmp_path)
    try:
        r = await c.get("/_gate/custom-profiles")
        assert r.status == 200 and (await r.json())["items"] == []
        r = await c.post("/_gate/custom-profiles", json={"yaml": GOOD})
        assert r.status == 200, await r.text()
        assert (await r.json())["profile"]["ai"]["type"] == "llm"
        assert (tmp_path / "custom_profiles/custom-tiny-llm/profile.yaml").is_file()
        r = await c.get("/_gate/profiles")
        assert "custom-tiny-llm" in [p["id"] for p in (await r.json())["profiles"]]
        items = (await (await c.get("/_gate/custom-profiles")).json())["items"]
        assert [i["id"] for i in items] == ["custom-tiny-llm"] and items[0]["yaml"] == GOOD
        # what a gate restart / reload sees
        assert set(load_custom(str(tmp_path), {"tiny"})[0]) == {"custom-tiny-llm"}
        app[SESSION].reload_profiles()
        assert "custom-tiny-llm" in app[SESSION].profiles
        # rejected: built-in id, missing prefix, credentials, bad dest, too large, not YAML
        for bad in (GOOD.replace("custom-tiny-llm", "tiny"), GOOD.replace("id: custom-tiny-llm", "id: tiny2"),
                    GOOD.replace("license: mit", "token: abc"), GOOD.replace("llm/tiny.gguf", "../x.gguf"),
                    GOOD + "#" * 70000, "id: [", ""):
            r = await c.post("/_gate/custom-profiles", json={"yaml": bad})
            assert r.status in (400, 413), bad[:60]
        # in use -> 409; asleep -> deleted
        s = app[SESSION]
        r = await c.post("/_gate/wake", json={"profile": "custom-tiny-llm"})
        assert r.status == 202
        r = await c.delete("/_gate/custom-profiles/custom-tiny-llm")
        assert r.status == 409
        await s.stop("test")
        r = await c.delete("/_gate/custom-profiles/custom-tiny-llm")
        assert r.status == 200 and "custom-tiny-llm" not in s.profiles
        assert (await c.delete("/_gate/custom-profiles/tiny")).status == 404
    finally:
        await c.close()


ANY_MODEL = """id: custom-any
title: Any model
comfyui: {repo: https://github.com/comfyanonymous/ComfyUI, commit: 2c75dc1b}
models:
- {id: f1, dest: loras/a.safetensors, url: hf://o/r/a.safetensors, license: mit, size_gb: 0.2, core: true}
wb_entry: {template: my-template}
"""


def test_custom_save_checks_and_core_plan(tmp_path):
    import pytest
    from gate.config import Settings
    from gate.plan import build_plan
    from gate.profiles import ProfileError, save_custom

    sd = str(tmp_path)
    prof = save_custom(sd, ANY_MODEL)
    plan = build_plan(prof, Settings.from_env({"AUTH_DISABLED": "true", "STATE_DIR": sd}))
    assert [m["dest"] for m in plan["models"]] == ["loras/a.safetensors"] and plan["models"][0]["core"] is True
    for bad, why in ((ANY_MODEL.replace(", core: true", ""), "no model"),
                     (ANY_MODEL.replace("wb_entry: {template: my-template}\n", ""), "nothing to use"),
                     (ANY_MODEL.replace("size_gb: 0.2", "size_gb: 90"), "GB")):
        with pytest.raises(ProfileError, match=why):
            save_custom(sd, bad)


async def test_custom_profiles_owner_only(tmp_path):
    from tests.test_app import client

    c, _ = await client(tmp_path, OWNER_EMAILS="boss@example.com")
    try:
        assert (await c.get("/_gate/custom-profiles")).status == 403
        assert (await c.post("/_gate/custom-profiles", json={"yaml": GOOD})).status == 403
    finally:
        await c.close()
'''

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

PROFILE_QWEN_VISION = """\
# Optional vision profile. Keep the existing text-only profile unchanged.
id: qwen38-27b-vision
title: Qwen3.8 27B · Q8_0 对话与看图
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
  url: hf://huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF@1c69098e9c32db6dc06fc48f1d6cc75ad07da89e/Huihui-Qwen3.8-27B-abliterated-Q8_0.gguf
  sha256: 427b9416c2aba9d21da4e90ddac50a2bc8b6d0bfef675af3580a43aa38e000c3
  license: apache-2.0
  size_gb: 29.05
- id: qwen38-vision-projector
  dest: llm/mmproj-model-bf16.gguf
  url: hf://huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF@1c69098e9c32db6dc06fc48f1d6cc75ad07da89e/mmproj-model-bf16.gguf
  sha256: c9a09064683620bea3d3bfed5d4462e1a97a7d2fff7e5045d6862a0a85eeb5b5
  license: apache-2.0
  size_gb: 0.94
ai:
  type: llm
  title: Qwen3.8 27B · 对话与看图
  llm:
    model: qwen38-27b-q8
    projector: qwen38-vision-projector
    name: qwen3.8-27b-vision
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
