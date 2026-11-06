#!/usr/bin/env python3
"""Let comfy-gate pick the Colab GPU per wake: POST /_gate/wake {"accelerator": "H100"}.

Run on the VPS from the repo root (e.g. /opt/comfy-colab-kit):
    sudo python3 gate-gpu-select.py
then:  sudo docker restart comfy-gate

- Without "accelerator" in the request the gate still uses COLAB_ACCELERATOR from .env.
- /_gate/status gains "accelerator" (this session's GPU) and "default_accelerator".
- Safe to run twice; keeps a .bak copy of every file it changes; aborts without
  touching anything if the code doesn't look as expected.
"""
import pathlib
import shutil
import sys

ROOT = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
GATE = ROOT / "apps" / "gate"
MARK = "# wb-gpu-select"

SESSION = [
    # __init__: field
    ('        self.machine.listeners.append(self._on_state_event)\n',
     '        self.accelerator = ""  ' + MARK + '\n        self.machine.listeners.append(self._on_state_event)\n'),
    # persist
    ('"stage_durations": self.stage_durations}))',
     '"stage_durations": self.stage_durations, "accelerator": self.accelerator}))'),
    # restore
    ('            self.stage_durations = d.get("stage_durations") or {}\n',
     '            self.stage_durations = d.get("stage_durations") or {}\n'
     '            self.accelerator = str(d.get("accelerator") or "")\n'),
    # status
    ('            "server_time": now,\n',
     '            "server_time": now,\n'
     '            "accelerator": (self.accelerator if m.state != State.SLEEPING and self.accelerator\n'
     '                            else self.settings.colab_accelerator),\n'
     '            "default_accelerator": self.settings.colab_accelerator,\n'),
    # wake(): accept + remember
    ('workflows: list[str] | None = None) -> WakeResult:',
     'workflows: list[str] | None = None,\n                   accelerator: str | None = None) -> WakeResult:'),
    ('            m.profile, self.workflows, self.progress = pid, workflows, {}\n',
     '            m.profile, self.workflows, self.progress = pid, workflows, {}\n'
     '            self.accelerator = (accelerator or "").strip().upper() or self.settings.colab_accelerator\n'),
    # switch keeps the current GPU
    ('        return await self.wake(profile_id, by=by, ip=ip)\n',
     '        return await self.wake(profile_id, by=by, ip=ip, accelerator=self.accelerator or None)\n'),
    # _boot: allocate the chosen GPU
    ('f"allocating Colab {self.settings.colab_accelerator} VM …"',
     'f"allocating Colab {self.accelerator or self.settings.colab_accelerator} VM …"'),
    ('await self.colab.new(name, self.settings.colab_accelerator)',
     'await self.colab.new(name, self.accelerator or self.settings.colab_accelerator)'),
]

APP = [
    ('        res = await s.wake(profile, by=ident.email, ip=client_ip, workflows=body.get("workflows"))\n',
     '        acc = str(body.get("accelerator") or "").strip().upper()  ' + MARK + '\n'
     '        if acc and acc not in ("T4", "L4", "G4", "A100", "H100", "V5E1", "V6E1"):\n'
     '            return err(400, "bad_accelerator", f"unknown accelerator {acc!r}")\n'
     '        res = await s.wake(profile, by=ident.email, ip=client_ip, workflows=body.get("workflows"),\n'
     '                           accelerator=acc or None)\n'),
]


def patch(path: pathlib.Path, edits):
    src = path.read_text(encoding="utf-8")
    if MARK in src:
        print(f"= {path.relative_to(ROOT)} already patched")
        return None
    out = src
    for old, new in edits:
        n = out.count(old)
        if n != 1:
            sys.exit(f"✗ {path.relative_to(ROOT)}: expected 1 match, found {n}:\n    {old.strip()[:90]}\n"
                     "Nothing was changed. Send this output to Claude.")
        out = out.replace(old, new)
    return out


def main():
    if not (GATE / "session.py").is_file():
        sys.exit(f"✗ {GATE}/session.py not found – run this from /opt/comfy-colab-kit")
    plans = [(GATE / "session.py", patch(GATE / "session.py", SESSION)),
             (GATE / "app.py", patch(GATE / "app.py", APP))]
    for p, new in plans:
        if new is None:
            continue
        shutil.copy2(p, p.with_suffix(".py.bak"))
        p.write_text(new, encoding="utf-8")
        try:
            compile(new, str(p), "exec")
        except SyntaxError as e:
            shutil.copy2(p.with_suffix(".py.bak"), p)
            sys.exit(f"✗ {p.name} did not compile, restored original: {e}")
        print(f"✓ patched {p.relative_to(ROOT)} (backup: {p.name}.bak)")
    print("Done. Now run:  sudo docker restart comfy-gate")


main()
