"""Build static Linux binaries, portable archives and per-architecture update manifests."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[1]

def main():
    p = argparse.ArgumentParser()
    p.add_argument("--go", default="go")
    p.add_argument("--out", default=str(ROOT / "dist"))
    p.add_argument("--label", default="", help="Artifact suffix for local test builds")
    args = p.parse_args()
    version = re.search(r'const AppVersion = "([^"]+)"', (ROOT / "app/update.go").read_text(encoding="utf-8")).group(1)
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    repo = os.environ.get("GITHUB_REPOSITORY", "a3974892-cmd/wakeloom")
    sums = []
    for arch in ("amd64", "arm64"):
        name = f"Wakeloom-{version}{args.label}-linux-{arch}"
        binary = out / name
        env = dict(os.environ, GOOS="linux", GOARCH=arch, CGO_ENABLED="0")
        subprocess.run([args.go, "build", "-trimpath", "-ldflags", "-s -w", "-o", str(binary), "."], cwd=ROOT / "app", env=env, check=True)
        binary.chmod(0o755)
        payload = binary.read_bytes()
        assert payload[:4] == b"\x7fELF"
        assert int.from_bytes(payload[18:20], "little") == {"amd64": 62, "arm64": 183}[arch]
        digest = hashlib.sha256(payload).hexdigest()
        archive = out / (name + ".tar.gz")
        with tarfile.open(archive, "w:gz") as tar:
            for member, data, mode in [
                ("wakeloom", payload, 0o755),
                ("README.md", (ROOT / "docs/LINUX.md").read_bytes(), 0o644),
                ("LICENSE", (ROOT / "LICENSE").read_bytes(), 0o644),
            ]:
                info = tarfile.TarInfo(member)
                info.size, info.mode = len(data), mode
                tar.addfile(info, io.BytesIO(data))
        # Local test builds do not advertise an unpublished URL to the updater.
        if not args.label:
            manifest = dict(version=version, os="linux", arch=arch, size=len(payload), sha256=digest,
                            url=f"https://github.com/{repo}/releases/download/v{version}/{name}",
                            notes="Linux 版本；浏览器工作台，本地直连与 VPS 网关。")
            (out / f"latest-linux-{arch}.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
        sums.extend([f"{digest}  {name}", f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}"])
        print(f"Built {name} ({len(payload)} bytes)")
    (out / "SHA256SUMS-linux.txt").write_text("\n".join(sums) + "\n", encoding="utf-8")

if __name__ == "__main__":
    main()
