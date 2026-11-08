"""Isolated Linux startup/HTTP/lock smoke check; never uses personal configuration."""
import os
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading
import urllib.request

binary = str(Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory() as directory:
    env = dict(os.environ, HOME=directory, XDG_CONFIG_HOME=directory, CKWB_HEADLESS="1", CKWB_PORT="")
    proc = subprocess.Popen([binary], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        lines = queue.Queue()
        threading.Thread(target=lambda: lines.put(proc.stdout.readline()), daemon=True).start()
        url = lines.get(timeout=20).strip()
        assert url.startswith("http://127.0.0.1:") and "?k=" in url
        with urllib.request.urlopen(url, timeout=10) as response:
            assert response.status == 200 and b"Wakeloom" in response.read()
        second = subprocess.run([binary], env=env, capture_output=True, timeout=5)
        assert second.returncode != 0, "duplicate headless instance accepted"
    finally:
        proc.terminate()
        proc.wait(timeout=10)
    restarted = subprocess.Popen([binary], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        lines = queue.Queue()
        threading.Thread(target=lambda: lines.put(restarted.stdout.readline()), daemon=True).start()
        assert lines.get(timeout=20).startswith("http://127.0.0.1:")
    finally:
        restarted.terminate()
        restarted.wait(timeout=10)
print("PASS: isolated Linux HTTP startup, duplicate prevention, restart after termination")
