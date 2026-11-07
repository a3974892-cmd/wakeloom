"""Generate gate-ai.py (one-click patch) from edits.py."""
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).parent))
import edits as E  # noqa: E402

out = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else str(pathlib.Path(__file__).resolve().parent.parent / "gate-ai.py"))
body = f'''#!/usr/bin/env python3
"""comfy-colab-kit: "AI" profiles for the workbench – local LLM (Ollama + GGUF) and image checkpoints.

Adds an optional `ai:` block to profiles, an /wbllm/... route inside ComfyUI that reaches the VM's
Ollama server, ready-made profiles (Qwen3.8 27B abliterated Q8_0, RealVisXL V5.0, Chroma1-HD), and
user-added profiles (GET/POST/DELETE /_gate/custom-profiles, stored in STATE_DIR/custom_profiles).

VPS (as root):   python3 gate-ai.py /opt/comfy-colab-kit
                 then: cd /opt/comfy-colab-kit && docker compose -f deploy/docker-compose.yml up -d --force-recreate

Safe to run twice; keeps .bak copies; aborts without changing anything if the code doesn't look as expected.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
PROFILES = {E.PROFILES!r}
PLAN = {E.PLAN!r}
SETUP = {E.SETUP!r}
APP = {E.APP!r}
SESSION = {E.SESSION!r}
TEST_CUSTOM = {E.TEST_CUSTOM!r}
NEW_PROFILES = {{"qwen38-27b-chat": {E.PROFILE_QWEN!r}, "realvisxl-v5": {E.PROFILE_REALVIS!r}, "chroma1-hd": {E.PROFILE_CHROMA!r}}}


def main():
    if (ROOT / "apps/gate/profiles.py").is_file():
        layout = {{"profiles": "apps/gate/profiles.py", "plan": "apps/gate/plan.py", "setup": "packages/colab/setup.py",
                  "app": "apps/gate/app.py", "session": "apps/gate/session.py"}}
        prof_dir, env_file = ROOT / "profiles/custom", ROOT / ".env"
    elif (ROOT / "gate/profiles.py").is_file():
        layout = {{"profiles": "gate/profiles.py", "plan": "gate/plan.py", "setup": "colab/setup.py"}}
        prof_dir, env_file = ROOT / "profiles", None
    else:
        sys.exit(f"✗ {{ROOT}} is not the comfy-colab-kit folder – run: python3 gate-ai.py /opt/comfy-colab-kit")
    todo = []
    for key, edits in (("profiles", PROFILES), ("plan", PLAN), ("setup", SETUP), ("app", APP), ("session", SESSION)):
        if key not in layout:
            continue  # the embedded kit has no gate server (app.py / session.py)
        p = ROOT / layout[key]
        src = p.read_text(encoding="utf-8")
        out, changed = src, 0
        for e in edits:
            old, new = e[0], e[1]
            region = e[2] if len(e) > 2 else None
            if new in out:
                continue  # this edit is already in place
            if region and region[0] in out:
                i = out.index(region[0])
                j = out.index(region[1], i)
                seg = new[new.find(region[0]):] if region[0] in new else new  # new may start with its anchor
                body = seg[:-len(region[1])] if seg.endswith(region[1]) else seg
                out = out[:i] + body + out[j:]
                changed += 1
                continue
            n = out.count(old)
            if n != 1:
                sys.exit(f"✗ {{layout[key]}}: expected 1 match, found {{n}}:\\n    {{old.strip()[:90]}}\\n"
                         "Nothing was changed. Send this output to Claude.")
            out = out.replace(old, new)
            changed += 1
        if not changed:
            print(f"= {{layout[key]}} up to date")
            continue
        try:
            compile(out, str(p), "exec")
        except SyntaxError as e:
            sys.exit(f"✗ {{layout[key]}} would not compile ({{e}}). Nothing was changed. Send this output to Claude.")
        todo.append((p, out, changed))
    for p, out, changed in todo:
        p.with_suffix(p.suffix + ".bak").write_bytes(p.read_bytes())
        p.write_text(out, encoding="utf-8")
        print(f"✓ patched {{p.relative_to(ROOT)}} ({{changed}} change(s), backup: {{p.name}}.bak)")
    if "app" in layout and (ROOT / "tests").is_dir():
        (ROOT / "tests/test_wb_custom_profiles.py").write_text(TEST_CUSTOM, encoding="utf-8")
        print("✓ tests/test_wb_custom_profiles.py")
    added = []
    for pid, text in NEW_PROFILES.items():
        d = prof_dir / pid
        d.mkdir(parents=True, exist_ok=True)
        (d / "profile.yaml").write_text(text, encoding="utf-8")
        added.append(pid)
        print(f"✓ profile {{d.relative_to(ROOT)}}/profile.yaml")
    if env_file is not None:
        env = env_file.read_text(encoding="utf-8") if env_file.is_file() else ""
        m = re.search(r"^PROFILES=(.*)$", env, re.M)
        cur = [x.strip() for x in (m.group(1) if m else "").split(",") if x.strip()]
        if not cur:
            # the gate's built-in default list – keep it so nothing disappears
            cur = ["profiles/examples/moss-image", "profiles/examples/sd-latest", "profiles/examples/qwen-image"]
        want = cur + [f"profiles/custom/{{p}}" for p in added if f"profiles/custom/{{p}}" not in cur]
        line = "PROFILES=" + ",".join(want)
        if m:
            env = env[:m.start()] + line + env[m.end():]
        else:
            env = env.rstrip("\\n") + "\\n" + line + "\\n"
        if want != cur:
            (env_file.parent / ".env.bak").write_text(env_file.read_text(encoding="utf-8") if env_file.is_file() else "")
            env_file.write_text(env, encoding="utf-8")
            print(f"✓ .env {{line}}")
        else:
            print("= .env PROFILES already lists the new profiles")
        print("Done. Now run:\\n  cd " + str(ROOT) + " && docker compose -f deploy/docker-compose.yml up -d --force-recreate")
    else:
        print("Done.")


main()
'''
out.write_text(body, encoding="utf-8")
compile(body, str(out), "exec")
print("wrote", out)
