"""Narrow Ollama installer patch. Reuse the tested host discovery/backup/atomic write code."""
from bootstrap_patch import TEMPLATE

HEADER = '''#!/usr/bin/env python3
"""Fix Ollama HTTP 504 downloads for the next Colab startup.

python3 gate-ollama-download.py --container comfy-gate
Only the recognized Ollama installer is changed. No bootstrap/profile edits, gate restart or GPU action.
"""
import argparse
import ast
import datetime
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile

SOURCE = __WB_SOURCE__
LEGACY = __WB_LEGACY__
MARKER = "# ── wb-ollama-download:"
FINISH = "# ── wb-ollama-download end ──"


def patch_source(text):
    crlf = "\\r\\n" in text
    updated = patch_source_lf(text.replace("\\r\\n", "\\n"))
    return updated.replace("\\n", "\\r\\n") if crlf else updated


def patch_source_lf(text):
    tree = ast.parse(text)
    installers = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "ollama_install"]
    if len(installers) != 1:
        raise RuntimeError("Expected one known Ollama installer; nothing changed")
    if SOURCE.strip() in text:
        return text
    if MARKER in text or FINISH in text:
        raise RuntimeError("Different Ollama download patch detected; preserve it for review instead of overwriting")
    original = installers[0]
    if ast.dump(original, include_attributes=False) != ast.dump(ast.parse(LEGACY).body[0], include_attributes=False):
        raise RuntimeError("Custom Ollama installer detected; refusing to overwrite it")
    lines = text.splitlines(keepends=True)
    first = sum(map(len, lines[:original.lineno - 1]))
    last = sum(map(len, lines[:original.end_lineno]))
    updated = text[:first] + SOURCE.strip() + "\\n" + text[last:]
    compile(updated, "setup.py", "exec")
    return updated
'''


def generate(source, legacy):
    # Tail contains only host-file discovery, backed-up atomic writes, and argument parsing.
    tail = TEMPLATE[TEMPLATE.index("\ndef discover("):]
    tail = tail.replace(".wb-bootstrap-", ".wb-ollama-download-")
    tail = tail.replace("Validated bootstrap update:", "Validated Ollama download update:")
    tail = tail.replace("Done. Next GPU wake uses named bootstrap logs and bounded retries.",
                        "Done. Next GPU wake uses verified Ollama official downloads with fallback.")
    return HEADER.replace("__WB_SOURCE__", repr(source)).replace("__WB_LEGACY__", repr(legacy)) + tail
