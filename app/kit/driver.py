"""ComfyWorkbench local mode: build the comfy-colab-kit plan on the VM, then run setup.py.

/content/kit/request.json (written by the workbench) selects the profile, workflows and library
extras; the plan is produced by the kit's own gate/plan.py so local mode boots exactly like the gate.
"""
import json
import os
import runpy
import subprocess
import sys

KIT = "/content/kit"
sys.path.insert(0, KIT)

req = json.load(open(f"{KIT}/request.json"))
try:
    import aiohttp  # noqa: F401  (auth proxy in front of ComfyUI)
except ImportError:
    subprocess.run([sys.executable, "-m", "pip", "install", "-q", "aiohttp"], check=False)
try:
    import yaml  # noqa: F401
except ImportError:
    subprocess.run([sys.executable, "-m", "pip", "install", "-q", "pyyaml"], check=False)

from gate import library  # noqa: E402
from gate.config import Settings  # noqa: E402
from gate.plan import build_plan, build_fetch_plan  # noqa: E402
from gate.profiles import load_profile  # noqa: E402

settings = Settings.from_env({"AUTH_DISABLED": "true", **{k: str(v) for k, v in (req.get("settings") or {}).items()}})
profile = load_profile(f"{KIT}/profiles/{req['profile']}")

if req.get("fetch_workflows"):
    plan = build_fetch_plan(profile, settings, list(req["fetch_workflows"]))
elif req.get("install"):
    plan = library.fetch_plan(build_plan(profile, settings), req["install"])
else:
    plan = build_plan(profile, settings, req.get("workflows") or None)
    added = library.merge_into_boot(plan, req.get("extras") or [])
    if added:
        print("KIT_LIB boot extras " + json.dumps(added), flush=True)

json.dump(plan, open(f"{KIT}/plan.json", "w"))
os.environ["KIT_PLAN"] = f"{KIT}/plan.json"
os.environ["KIT_QUICK_TUNNEL"] = "1"
os.environ["KIT_MODEL_STORE"] = settings.model_store
os.environ["KIT_DL_CONNS"] = str(settings.download_connections)
runpy.run_path(f"{KIT}/colab/setup.py", run_name="__main__")
