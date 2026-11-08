"""VPS helper: run the vision installer in the current Colab VM without restarting the gate.

Usage: python3 enable-vision-on-vps.py /opt/comfy-colab-kit
Keep enable-qwen-vision.py beside this file. Requires the gateway's existing Docker/Colab login.
"""
import json
from pathlib import Path
import subprocess
import sys


def container_code(source):
    vm_code = "exec(compile(" + repr(source) + ", '/content/wb_enable_vision.py', 'exec'), {'__name__':'__main__'})"
    return '''import asyncio, contextlib, json, urllib.request
from pathlib import Path
from gate.config import Settings, read_secret_file
from gate.colab import ColabCLI
s = Settings.from_env()
state = json.loads((Path(s.state_dir) / 'session.json').read_text())
if state.get('state') != 'ready':
    raise RuntimeError('GPU must already be ready; no VM is started or stopped by this helper')
token = read_secret_file(s.agent_token_file).strip() if s.agent_token_file else ''
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def touch():
    request = urllib.request.Request(f'http://127.0.0.1:{s.listen_port}/wbllm/info', headers={'Authorization':'Bearer '+token})
    with opener.open(request, timeout=15) as response:
        json.load(response)
touch()  # Verify authentication before changing anything in the VM.
async def keepalive():
    while True:
        await asyncio.sleep(15)
        await asyncio.to_thread(touch)
async def run():
    cli = ColabCLI(s.colab_bin, s.colab_token_dir)
    heartbeat = asyncio.create_task(keepalive())
    try:
        async for line in cli.exec_stream(s.colab_session_name, VM_CODE, 2700):
            print(line, flush=True)
    finally:
        heartbeat.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await heartbeat
asyncio.run(run())
'''.replace("VM_CODE", repr(vm_code))


def main():
    root = Path(sys.argv[1] if len(sys.argv) > 1 else "/opt/comfy-colab-kit").resolve()
    compose = root / "deploy/docker-compose.yml"
    if not compose.is_file():
        raise SystemExit("Gateway folder not found; provide its actual install directory")
    source = Path(__file__).with_name("enable-qwen-vision.py").read_text(encoding="utf-8")
    print("Using the gateway's existing Colab session; the GPU and gateway stay running", flush=True)
    result = subprocess.run(["docker", "compose", "-f", str(compose), "exec", "-T", "comfy-gate", "python", "-"],
                            cwd=root, input=container_code(source), text=True, timeout=3000)
    raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
