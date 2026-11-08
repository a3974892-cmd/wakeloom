"""Canonical Ollama installer source shared by the embedded kit and the narrow VPS patch.

Release and checksum: https://github.com/ollama/ollama/releases/tag/v0.40.15
The versioned ollama.com URL is also supported by the official install.sh.
"""

LEGACY_INSTALL_SOURCE = r'''def ollama_install():
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
                    except TypeError:
                        tf.extractall("/usr/local")
            if os.path.isfile("/usr/local/bin/ollama"):
                os.chmod("/usr/local/bin/ollama", 0o755)
                note("installed /usr/local/bin/ollama")
                return
            last = RuntimeError("bin/ollama missing from the archive")
        except Exception as e:
            last = e
        note(f"attempt {attempt} failed: {last!r}")
        time.sleep(5 * attempt)
    raise RuntimeError(f"Ollama install failed: {last!r}")
'''

INSTALL_SOURCE = r'''
# ── wb-ollama-download: verified release, official fallback and visible progress ──
OLLAMA_INSTALL_VERSION = "0.40.15"
OLLAMA_INSTALL_PREFIX = "/usr/local"
OLLAMA_INSTALL_SIZE = 1543547172
OLLAMA_INSTALL_SHA256 = "628c018404a4206493efd38e46251c154ede5141b8b8dc85c659b3b027c47379"
OLLAMA_INSTALL_URLS = (
    "https://api.github.com/repos/ollama/ollama/releases/assets/715495290",
    "https://github.com/ollama/ollama/releases/download/v0.40.15/ollama-linux-amd64.tar.zst",
    "https://ollama.com/download/ollama-linux-amd64.tar.zst?version=0.40.15",
)


def _ollama_install_archive(dest, note):
    import hashlib
    import urllib.parse
    errors = []
    for round_no in range(1, 3):
        for url in OLLAMA_INSTALL_URLS:
            host = urllib.parse.urlsplit(url).hostname
            note(f"download {host}, attempt {round_no}/2, version {OLLAMA_INSTALL_VERSION}")
            try:
                request = urllib.request.Request(url, headers={"User-Agent": "wakeloom-kit", "Accept": "application/octet-stream", "Cache-Control": "no-cache"})
                with urllib.request.urlopen(request, timeout=30) as response, open(dest, "wb") as out:
                    digest = hashlib.sha256()
                    done, started, last = 0, time.monotonic(), time.monotonic()
                    while True:
                        block = response.read(8 << 20)
                        if not block:
                            break
                        done += len(block)
                        if done > OLLAMA_INSTALL_SIZE:
                            raise RuntimeError("release exceeds its pinned size")
                        out.write(block)
                        digest.update(block)
                        if time.monotonic() - last >= 5:
                            note(f"downloaded {done / 1e6:.0f}/{OLLAMA_INSTALL_SIZE / 1e6:.0f} MB")
                            last = time.monotonic()
                        if time.monotonic() - started > 360:
                            raise TimeoutError("release download exceeded 360 seconds")
                if done != OLLAMA_INSTALL_SIZE or digest.hexdigest() != OLLAMA_INSTALL_SHA256:
                    raise RuntimeError("release size/SHA-256 mismatch; nothing installed")
                note("release size and SHA-256 verified")
                return
            except Exception as error:
                code = getattr(error, "code", None)
                detail = f"HTTP {code}" if code else type(error).__name__ + ": " + str(error)[:180]
                errors.append(f"{host}: {detail}")
                note(f"{host} failed: {detail}; trying the next official source")
        if round_no < 2:
            time.sleep(3)
    raise RuntimeError("Ollama official downloads failed: " + "; ".join(errors[-4:]))


def _ollama_install_extract(archive, stage, note):
    import tarfile
    import pathlib
    import zstandard
    root = os.path.realpath(stage)
    last = time.monotonic()
    with open(archive, "rb") as source, zstandard.ZstdDecompressor().stream_reader(source) as reader, \
            tarfile.open(fileobj=reader, mode="r|") as tar:
        for member in tar:
            name = pathlib.PurePosixPath(member.name)
            if name.is_absolute() or ".." in name.parts or "\\" in member.name or ":" in member.name:
                raise RuntimeError("unsafe path in Ollama release")
            target = os.path.realpath(os.path.join(root, *name.parts))
            if os.path.commonpath([root, target]) != root or member.isdev() or member.isfifo():
                raise RuntimeError("unsafe entry in Ollama release")
            if member.issym() or member.islnk():
                link = pathlib.PurePosixPath(member.linkname)
                base = os.path.dirname(target) if member.issym() else root
                resolved = os.path.realpath(os.path.join(base, member.linkname))
                if link.is_absolute() or os.path.commonpath([root, resolved]) != root:
                    raise RuntimeError("unsafe link in Ollama release")
            try:
                tar.extract(member, stage, filter="data")
            except TypeError:  # Older Python; path/type/link checks above still apply.
                tar.extract(member, stage)
            if time.monotonic() - last >= 5:
                note("extracting the verified Ollama release")
                last = time.monotonic()
    binary = os.path.join(stage, "bin", "ollama")
    if not os.path.isfile(binary) or os.path.getsize(binary) == 0:
        raise RuntimeError("bin/ollama missing or empty in verified release")
    if not os.path.isdir(os.path.join(stage, "lib", "ollama")):
        raise RuntimeError("lib/ollama missing in verified release")
    os.chmod(binary, 0o755)


def _ollama_install_publish(stage, prefix, note):
    import tempfile
    env = {k: v for k, v in os.environ.items() if k not in ("HF_TOKEN", "CF_TUNNEL_TOKEN")}
    binary = os.path.join(stage, "bin", "ollama")
    checked = subprocess.run([binary, "--version"], env=env, stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT, timeout=30)
    version = checked.stdout.decode("utf-8", "replace")
    if checked.returncode or OLLAMA_INSTALL_VERSION not in version:
        raise RuntimeError("downloaded Ollama executable failed its version check")
    # Stage and backup are on the installation filesystem. Restore old paths if publishing fails.
    with tempfile.TemporaryDirectory(prefix=".wb-ollama-previous-", dir=prefix) as previous:
        changes = []
        try:
            for relative in ("lib/ollama", "bin/ollama"):
                target = os.path.join(prefix, relative)
                old = os.path.join(previous, relative)
                os.makedirs(os.path.dirname(target), exist_ok=True)
                os.makedirs(os.path.dirname(old), exist_ok=True)
                existed = os.path.lexists(target)
                if existed:
                    os.replace(target, old)
                changes.append((relative, existed))
                os.replace(os.path.join(stage, relative), target)
        except Exception:
            for relative, existed in reversed(changes):
                target, old = os.path.join(prefix, relative), os.path.join(previous, relative)
                if os.path.lexists(target):
                    os.replace(target, os.path.join(stage, relative))
                if existed:
                    os.replace(old, target)
            raise
    note("installed and checked Ollama " + OLLAMA_INSTALL_VERSION)


def ollama_install():
    """Use official asset/download routes for one pinned release; no install.sh, apt or systemd."""
    import tempfile
    os.makedirs(LOG, exist_ok=True)
    with open(os.path.join(LOG, "ollama_install.log"), "a", encoding="utf-8") as log:
        def note(message):
            message = "KIT_OLLAMA_INSTALL " + message
            log.write(message + "\n")
            log.flush()
            print(message, flush=True)

        try:
            import zstandard
        except ImportError:
            for attempt in (1, 2):
                note(f"installing archive decoder, attempt {attempt}/2")
                try:
                    result = subprocess.run([sys.executable, "-m", "pip", "install", "-q", "zstandard"],
                                            stdout=log, stderr=subprocess.STDOUT, timeout=90)
                    if result.returncode:
                        raise RuntimeError("archive decoder installer failed")
                    import zstandard
                    break
                except Exception:
                    if attempt == 2:
                        raise RuntimeError("Unable to install the zstandard archive decoder") from None
        prefix = OLLAMA_INSTALL_PREFIX
        with tempfile.TemporaryDirectory(prefix=".wb-ollama-install-", dir=prefix) as temporary:
            archive = os.path.join(temporary, "release.tar.zst")
            stage = os.path.join(temporary, "stage")
            os.makedirs(stage)
            _ollama_install_archive(archive, note)
            _ollama_install_extract(archive, stage, note)
            _ollama_install_publish(stage, prefix, note)
# ── wb-ollama-download end ──
'''
