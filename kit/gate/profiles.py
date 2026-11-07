"""Profile loading and validation.

A profile (examples/profiles/<id>/profile.yaml) declares which ComfyUI commit,
custom nodes, models and community workflows a session needs. Profiles are
public and must never contain secrets: any key that looks like a credential is
rejected, and URLs may not carry query strings (where tokens tend to hide).
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

FORBIDDEN_KEY = re.compile(r"(token|secret|password|passwd|api[_-]?key|authorization|cookie|credential)", re.I)
ID_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")
URL_RE = re.compile(r"^(https://[^\s?#]+|hf://[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(@[A-Za-z0-9_.-]+)?/[^\s?#]+)$")
MODEL_DIRS = {"checkpoints", "diffusion_models", "text_encoders", "clip", "vae", "loras", "upscale_models",
              "controlnet", "embeddings", "clip_vision", "unet", "rapid_moss", "model_patches", "llm",
              # wb-dirs: more ComfyUI model folders for user-added profiles
              "style_models", "gligen", "hypernetworks", "photomaker", "vae_approx", "audio_encoders",
              "latent_upscale_models", "ipadapter", "sams", "ultralytics", "insightface", "instantid"}


#: flags the kit sets itself (the tunnel and gate depend on them)
RESERVED_ARGS = {"--listen", "--port", "--output-directory", "--disable-auto-launch", "--extra-model-paths-config"}
RESERVED_ENV = {"CF_TUNNEL_TOKEN", "HF_TOKEN", "KIT_GATE_URL", "KIT_PROFILE_JSON"}


class ProfileError(ValueError):
    pass


@dataclass(frozen=True)
class Model:
    id: str
    dest: str
    url: str
    size_gb: float | None = None
    sha256: str | None = None
    license: str = ""
    core: bool = False


@dataclass(frozen=True)
class CustomNode:
    id: str
    url: str
    ref: str | None = None
    subdir: str | None = None
    license: str = ""


@dataclass(frozen=True)
class Workflow:
    id: str
    title: str
    url: str
    license: str
    format: str  # json | png
    models: tuple[str, ...]
    custom_nodes: tuple[str, ...] = ()
    app_inputs: tuple[tuple[Any, ...], ...] = ()
    app_outputs: tuple[Any, ...] = ()
    overrides: tuple[dict, ...] = ()
    optional: bool = False
    file: str | None = None
    source: str = ""
    loras: tuple[str, ...] = ()  # extra LoRA URLs (hf://…) downloaded into models/loras/
    prefetch: bool = False  # download this workflow's models in the background on every wake (else on demand)


@dataclass(frozen=True)
class Smoke:
    """An API-format workflow published by its author, queued once after boot."""
    api_url: str
    license: str
    models: tuple[str, ...] = ()
    custom_nodes: tuple[str, ...] = ()
    timeout_s: int = 600


@dataclass(frozen=True)
class Profile:
    id: str
    title: str
    path: Path
    accelerator: str
    comfyui_commit: str
    comfyui_args: tuple[str, ...]
    comfyui_settings: dict
    comfyui_env: dict
    extra_model_paths: dict | None
    frontend_package: str | None
    models: dict[str, Model]
    custom_nodes: dict[str, CustomNode]
    workflows: dict[str, Workflow]
    app_default: str | None
    smoke: "Smoke | None"
    raw: dict = field(repr=False, default_factory=dict)
    #: loader filename (as written in an upstream workflow) -> model id it is remapped to at install time
    model_aliases: dict = field(default_factory=dict)
    ai: dict | None = None  # wb-ai

    def models_for(self, workflow_ids: list[str] | None = None) -> list[Model]:
        wf_ids = workflow_ids or [w.id for w in self.workflows.values() if not w.optional]
        need: list[str] = []
        for wid in wf_ids:
            for mid in self.workflows[wid].models:
                if mid not in need:
                    need.append(mid)
        return [self.models[m] for m in need]

    def total_size_gb(self, workflow_ids: list[str] | None = None) -> float:
        return round(sum(m.size_gb or 0 for m in self.models_for(workflow_ids)), 2)

    def wake_size_gb(self) -> float:
        """What a plain wake downloads: the default workflow's models + the smoke test's (others are on demand)."""
        ids = {m.id for m in (self.models_for([self.app_default]) if self.app_default else [])}
        ids |= set(self.smoke.models if self.smoke else ())
        ids |= set((self.ai or {}).get("models") or ())
        used = {mid for w in self.workflows.values() for mid in w.models} | ids  # wb-core: see plan.py
        ids |= {m.id for m in self.models.values() if m.core and m.id not in used}
        return round(sum(self.models[i].size_gb or 0 for i in ids), 2)

    def summary(self) -> dict:
        return {
            "id": self.id,
            "title": self.title,
            "accelerator": self.accelerator,
            "comfyui_commit": self.comfyui_commit,
            "app_default": self.app_default,
            "ai": self.ai,
            "size_gb_default": self.wake_size_gb(),
            "size_gb_all_installed": self.total_size_gb(),
            "workflows": [{"id": w.id, "title": w.title, "optional": w.optional, "license": w.license,
                           "prefetch": w.prefetch, "size_gb": self.total_size_gb([w.id])}
                          for w in self.workflows.values()],
        }


def _scan_secrets(obj: Any, where: str = "") -> None:
    if isinstance(obj, dict):
        for k, v in obj.items():
            if FORBIDDEN_KEY.search(str(k)):
                raise ProfileError(f"forbidden key '{where}{k}': profiles must not contain credentials")
            _scan_secrets(v, f"{where}{k}.")
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            _scan_secrets(v, f"{where}{i}.")
    elif isinstance(obj, str) and re.search(r"(hf_[A-Za-z0-9]{20,}|ya29\.|ghp_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{20,}\.)", obj):
        raise ProfileError(f"value at '{where.rstrip('.')}' looks like a credential")


def _req(d: dict, key: str, ctx: str) -> Any:
    if key not in d or d[key] in (None, ""):
        raise ProfileError(f"{ctx}: missing '{key}'")
    return d[key]


def _url(u: str, ctx: str) -> str:
    if not URL_RE.match(u or ""):
        raise ProfileError(f"{ctx}: url must be https://… or hf://owner/repo[@rev]/path without query string, got {u!r}")
    return u


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


def parse_profile(data: dict, path: Path) -> Profile:
    if not isinstance(data, dict):
        raise ProfileError("profile must be a mapping")
    _scan_secrets(data)
    pid = _req(data, "id", "profile")
    if not ID_RE.match(pid):
        raise ProfileError(f"invalid profile id {pid!r}")
    models: dict[str, Model] = {}
    for i, m in enumerate(data.get("models") or []):
        ctx = f"{pid}.models[{i}]"
        mid, dest = _req(m, "id", ctx), _req(m, "dest", ctx)
        if mid in models:
            raise ProfileError(f"{ctx}: duplicate model id {mid}")
        top = dest.split("/", 1)[0]
        if top not in MODEL_DIRS or ".." in dest or dest.startswith("/"):
            raise ProfileError(f"{ctx}: dest must be <models subdir>/<file>, got {dest!r}")
        sha = m.get("sha256")
        if sha is not None and not re.fullmatch(r"[0-9a-fA-F]{64}", str(sha)):
            raise ProfileError(f"{ctx}: sha256 must be 64 hex chars")
        models[mid] = Model(mid, dest, _url(_req(m, "url", ctx), ctx), m.get("size_gb"), sha,
                            str(m.get("license", "")), bool(m.get("core", False)))
    nodes: dict[str, CustomNode] = {}
    for i, n in enumerate(data.get("custom_nodes") or []):
        ctx = f"{pid}.custom_nodes[{i}]"
        nid = _req(n, "id", ctx)
        nodes[nid] = CustomNode(nid, _url(_req(n, "url", ctx), ctx), n.get("ref"), n.get("subdir"), str(n.get("license", "")))
    workflows: dict[str, Workflow] = {}
    path = Path(path)
    for i, w in enumerate(data.get("workflows") or []):
        ctx = f"{pid}.workflows[{i}]"
        wid = _req(w, "id", ctx)
        if not ID_RE.match(wid):
            raise ProfileError(f"{ctx}: invalid id {wid!r}")
        fmt = w.get("format", "json")
        if fmt not in ("json", "png", "builtin"):
            raise ProfileError(f"{ctx}: format must be json|png|builtin")
        url = w.get("url", "")
        file = w.get("file")
        if file:
            # a community workflow committed under <profile>/workflows/ (must still carry source + license)
            if ".." in file or file.startswith("/") or not file.startswith("workflows/"):
                raise ProfileError(f"{ctx}: file must be a relative path under workflows/")
            if not (path / file).is_file():
                raise ProfileError(f"{ctx}: file {file} not found")
        elif fmt != "builtin":
            _url(url, ctx)
        loras = tuple(_url(u, ctx) for u in (w.get("loras") or []))
        lic = _req(w, "license", ctx)
        _req(w, "source", ctx)
        wm = tuple(w.get("models") or [])
        for mid in wm:
            if mid not in models:
                raise ProfileError(f"{ctx}: unknown model '{mid}'")
        wn = tuple(w.get("custom_nodes") or [])
        for nid in wn:
            if nid not in nodes:
                raise ProfileError(f"{ctx}: unknown custom node '{nid}'")
        app = w.get("app") or {}
        inputs = tuple(tuple(x) for x in app.get("inputs") or [])
        outputs = tuple(app.get("outputs") or [])
        # outputs may be empty: some official templates keep SaveImage inside a subgraph,
        # in which case App mode shows the subgraph's own outputs.
        workflows[wid] = Workflow(wid, str(w.get("title", wid)), url, str(lic), fmt, wm, wn, inputs, outputs,
                                  tuple(w.get("overrides") or []), bool(w.get("optional", False)),
                                  file, str(w.get("source", "")), loras, bool(w.get("prefetch", False)))
    # a scaffold profile may have no workflows yet; then app_default is optional
    app_default = data.get("app_default") or (None if not workflows else _req(data, "app_default", pid))
    if workflows and app_default not in workflows:
        raise ProfileError(f"{pid}: app_default '{app_default}' is not a workflow id")
    smoke_raw = data.get("smoke") or None
    smoke = None
    if smoke_raw:
        ctx = f"{pid}.smoke"
        smoke = Smoke(_url(_req(smoke_raw, "api_url", ctx), ctx), str(_req(smoke_raw, "license", ctx)),
                      tuple(smoke_raw.get("models") or []), tuple(smoke_raw.get("custom_nodes") or []),
                      int(smoke_raw.get("timeout_s", 600)))
        for mid in smoke.models:
            if mid not in models:
                raise ProfileError(f"{ctx}: unknown model '{mid}'")
    comfy = data.get("comfyui") or {}
    args = comfy.get("args") or []
    if not isinstance(args, list) or not all(isinstance(a, str) for a in args):
        raise ProfileError(f"{pid}.comfyui.args must be a list of strings")
    for a in args:
        flag = a.split("=", 1)[0]
        if a.startswith("--") and flag in RESERVED_ARGS:
            raise ProfileError(f"{pid}.comfyui.args: {flag} is managed by the kit and cannot be overridden")
    if args and not args[0].startswith("--"):
        raise ProfileError(f"{pid}.comfyui.args must start with a --flag")
    settings = comfy.get("settings") or {}
    if not isinstance(settings, dict):
        raise ProfileError(f"{pid}.comfyui.settings must be a mapping")
    cenv = comfy.get("env") or {}
    if not isinstance(cenv, dict) or not all(re.fullmatch(r"[A-Z_][A-Z0-9_]*", str(k)) for k in cenv):
        raise ProfileError(f"{pid}.comfyui.env must map UPPER_CASE names to values")
    if any(k in RESERVED_ENV for k in cenv):
        raise ProfileError(f"{pid}.comfyui.env may not set {sorted(RESERVED_ENV & set(cenv))}")
    emp = comfy.get("extra_model_paths")
    if emp is not None and not isinstance(emp, dict):
        raise ProfileError(f"{pid}.comfyui.extra_model_paths must be a mapping (extra_model_paths.yaml content)")
    aliases = data.get("model_aliases") or {}
    if not isinstance(aliases, dict):
        raise ProfileError(f"{pid}.model_aliases must map a filename to a model id")
    for fname, mid in aliases.items():
        if mid not in models:
            raise ProfileError(f"{pid}.model_aliases: unknown model '{mid}' for '{fname}'")
    ai = _parse_ai(data.get("ai"), models, pid)
    return Profile(
        model_aliases={str(k): str(v) for k, v in aliases.items()},
        id=pid, title=str(data.get("title", pid)), path=path,
        accelerator=str(data.get("accelerator", "L4")),
        comfyui_commit=str(_req(comfy, "commit", f"{pid}.comfyui")),
        comfyui_args=tuple(args), comfyui_settings=dict(settings),
        comfyui_env={str(k): str(v) for k, v in cenv.items()}, extra_model_paths=emp,
        frontend_package=(data.get("frontend") or {}).get("package"),
        models=models, custom_nodes=nodes, workflows=workflows,
        app_default=app_default, smoke=smoke, raw=data, ai=ai,
    )


def load_profile(ref: str | Path, root: Path | None = None) -> Profile:
    p = Path(ref)
    if root and not p.is_absolute():
        p = root / p
    f = p / "profile.yaml" if p.is_dir() else p
    if not f.is_file() and root and "examples/profiles" in str(ref):
        alt = root / str(ref).replace("examples/profiles", "profiles/examples")
        alt_f = alt / "profile.yaml" if alt.is_dir() else alt
        if alt_f.is_file():
            f = alt_f
    if not f.is_file():
        raise ProfileError(f"profile not found: {f}")
    return parse_profile(yaml.safe_load(f.read_text()), f.parent)


def load_all(refs: list[str] | tuple[str, ...], root: Path) -> tuple[dict[str, Profile], dict[str, str]]:
    ok: dict[str, Profile] = {}
    errors: dict[str, str] = {}
    for r in refs:
        try:
            prof = load_profile(r, root)
            ok[prof.id] = prof
        except (ProfileError, yaml.YAMLError) as e:
            errors[r] = str(e)
    return ok, errors


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


def hf_to_https(url: str) -> str:
    """hf://owner/repo[@rev]/path -> https://huggingface.co/owner/repo/resolve/<rev>/path"""
    if not url.startswith("hf://"):
        return url
    owner, repo_rev, path = url[5:].split("/", 2)
    repo, _, rev = repo_rev.partition("@")
    return f"https://huggingface.co/{owner}/{repo}/resolve/{rev or 'main'}/{path}"
