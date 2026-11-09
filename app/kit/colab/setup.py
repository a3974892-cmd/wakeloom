# comfy-colab-kit — runs INSIDE the Colab kernel (started by comfy-gate via `colab exec`).
# Input:  KIT_PLAN=/content/kit/plan.json (built by gate/plan.py), optional CF_TUNNEL_TOKEN / HF_TOKEN in env.
# Output protocol on stdout (parsed by gate/session.py):
#   KIT_STAGE <name> start|done <secs>|fail      KIT_PROGRESS <key> <json>
#   KIT_READY <json timings>                     KIT_FAILED stage=<name> error=<repr>
# Ported and generalised from the original moss-colab-kit setup script.
import json
import os
import re
import shutil
import signal
import struct
import subprocess
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

_PLAN_PATH = os.environ.get("KIT_PLAN", "/content/kit/plan.json")
PLAN = json.load(open(_PLAN_PATH)) if os.path.exists(_PLAN_PATH) else {}
COMFY = "/content/ComfyUI"
LOG = "/content/kit_logs"
PORT = 8188
T0 = time.time()
TIMES = {}


def ts(label):
    """Absolute timestamp marker (KIT_TS <epoch> <label>). The gate forwards it as a plain log line."""
    print(f"KIT_TS {time.time():.1f} {label}", flush=True)


ts("import")
HB_FILE = "/content/kit_heartbeat"


class KitAbort(Exception):
    """A stage failed and KIT_FAILED was already printed."""


class Stage:
    def __init__(self, name):
        self.name = name

    def __enter__(self):
        self.t = time.time()
        print(f"KIT_STAGE {self.name} start", flush=True)
        return self

    def __exit__(self, et, ev, tb):
        d = time.time() - self.t
        TIMES[self.name] = round(d, 1)
        if et:
            print(f"KIT_STAGE {self.name} fail {d:.1f}", flush=True)
            print(f"KIT_FAILED stage={self.name} error={ev!r}"[:800], flush=True)
            # not sys.exit(): inside the Jupyter kernel that dumps a huge (and broken) IPython traceback
            raise KitAbort(self.name) from None
        print(f"KIT_STAGE {self.name} done {d:.1f}", flush=True)


def sh(cmd, check=True, quiet=False, env=None):
    if not quiet:
        print(f"$ {cmd}", flush=True)
    # subprocess output written straight to fd 1 bypasses the Jupyter kernel and never reaches `colab exec`;
    # pipe it through Python's print so the gate (and the wake page log) sees it
    p = subprocess.Popen(["bash", "-lc", cmd], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                         text=True, errors="replace")
    for line in p.stdout:
        print(line.rstrip("\n"), flush=True)
    rc = p.wait()
    if check and rc:
        raise RuntimeError(f"failed ({rc}): {cmd}")
    return rc


def up(path="/system_stats"):
    try:
        urllib.request.urlopen(f"http://127.0.0.1:{PORT}{path}", timeout=3)
        return True
    except Exception:
        return False


def fetch(url, timeout=60):
    req = urllib.request.Request(url, headers={"User-Agent": "comfy-colab-kit"})
    if os.environ.get("HF_TOKEN") and url.startswith("https://huggingface.co/"):
        req.add_header("Authorization", "Bearer " + os.environ["HF_TOKEN"])
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read()


HF_RE = re.compile(r"^https://huggingface\.co/([^/]+/[^/]+)/resolve/([^/]+)/(.+)$")


# ─────────────────────────── downloads ───────────────────────────
# HF files download in a child process each, so a stalled file can be killed/restarted and background (non-core)
# files can be paused (SIGSTOP) while ComfyUI is executing a prompt.
#
# The downloader is a hand-rolled multi-connection Range fetcher (stdlib only). Rationale, measured on the
# Colab L4 VM: huggingface_hub's classic path tops out around 130-145 MB/s per file, a plain GET reaches
# ~226 MB/s, and 8 parallel Range connections on the same object reach ~700 MB/s (the VM egress ceiling).
# `HF_HUB_ENABLE_HF_TRANSFER` is ignored by huggingface_hub >= 0.32 and Xet stays disabled on purpose
# (Xet-backed repos report 403/429 stalls on shared Colab IPs), so the Range fan-out is the fast path.
# Chunks land in <tmp>/<name>.parts/ so a killed file resumes where it stopped instead of restarting.
DL_SCRIPT = r"""
import os, sys, time, math, shutil, hashlib, threading, queue
import urllib.request, urllib.error, urllib.parse

REPO, FNAME, REV, TMP = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
EXPECT_SHA = (sys.argv[5] if len(sys.argv) > 5 else "")
CONNS = max(1, int(os.environ.get("KIT_DL_CONNS", "8")))
CHUNK = max(16, int(os.environ.get("KIT_DL_CHUNK_MB", "256"))) * 1024 * 1024
TOKEN = os.environ.get("HF_TOKEN") or ""
URL = "https://huggingface.co/{}/resolve/{}/{}".format(REPO, REV or "main", FNAME)


class _SafeRedirect(urllib.request.HTTPRedirectHandler):
    # Follow the HF -> CDN redirect, but never carry the HF token to the CDN host.
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None and urllib.parse.urlparse(newurl).netloc != urllib.parse.urlparse(req.full_url).netloc:
            new.headers = {k: v for k, v in new.headers.items() if k.lower() != "authorization"}
        return new


def total_size(url):
    # One probe request to learn the object size. The signed CDN URL it redirects to is deliberately
    # NOT reused: sharing one signed URL across parallel ranges is what throttles the CDN to ~65 MB/s.
    opener = urllib.request.build_opener(_SafeRedirect)
    hdrs = {"User-Agent": "comfy-colab-kit", "Range": "bytes=0-0", "Accept-Encoding": "identity"}
    if TOKEN:
        hdrs["Authorization"] = "Bearer " + TOKEN
    with opener.open(urllib.request.Request(url, headers=hdrs), timeout=60) as r:
        cr = r.headers.get("Content-Range")
        etag = (r.headers.get("ETag") or r.headers.get("x-linked-etag") or "").strip().strip('"')
        if r.status != 206 or not cr:
            raise RuntimeError("server ignored Range (status {})".format(r.status))
        return int(cr.rsplit("/", 1)[1]), etag


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for blk in iter(lambda: f.read(8 << 20), b""):
            h.update(blk)
    return h.hexdigest()


def fetch_range(url, total, out_path):
    # One disk write pass: chunks are written straight into the final file at their offsets.
    # A sidecar bitmap keeps resume state, so a killed download continues instead of restarting.
    nchunks = max(1, int(math.ceil(total / float(CHUNK))))
    bounds = [(i * CHUNK, min(total, (i + 1) * CHUNK) - 1) for i in range(nchunks)]
    bitmap = out_path + ".chunks"
    done = set()
    if os.path.exists(bitmap):
        with open(bitmap) as f:
            for line in f:
                line = line.strip()
                if line.isdigit():
                    done.add(int(line))
    os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)
    fd = os.open(out_path, os.O_RDWR | os.O_CREAT, 0o644)
    os.ftruncate(fd, total)
    state = {"done": sum(bounds[i][1] - bounds[i][0] + 1 for i in done), "inflight": 0}
    lock = threading.Lock()
    q = queue.Queue()
    for i, b in enumerate(bounds):
        if i not in done:
            q.put((i, b))
    errors = []

    def worker():
        while True:
            try:
                i, (start_b, end_b) = q.get_nowait()
            except queue.Empty:
                return
            need = end_b - start_b + 1
            have = 0
            attempt = 0
            while have < need and attempt < 6:
                attempt += 1
                try:
                    s = start_b + have
                    hdrs = {"User-Agent": "comfy-colab-kit", "Accept-Encoding": "identity",
                            "Range": "bytes={}-{}".format(s, end_b)}
                    if TOKEN:
                        hdrs["Authorization"] = "Bearer " + TOKEN
                    opener = urllib.request.build_opener(_SafeRedirect)
                    with opener.open(urllib.request.Request(url, headers=hdrs), timeout=120) as r:
                        if r.status != 206:
                            raise RuntimeError("no range support (status {})".format(r.status))
                        buf = b""
                        while True:
                            b = r.read(1 << 20)
                            if not b:
                                break
                            buf += b
                            if len(buf) >= (8 << 20):
                                os.pwrite(fd, buf, s + have)
                                have += len(buf)
                                with lock:
                                    state["inflight"] += len(buf)
                                buf = b""
                        if buf:
                            os.pwrite(fd, buf, s + have)
                            have += len(buf)
                            with lock:
                                state["inflight"] += len(buf)
                except Exception as e:
                    if attempt >= 6:
                        errors.append("chunk {}: {!r}".format(i, e))
                        break
                    time.sleep(min(20, 2 * attempt))
            if have >= need:
                with lock:
                    state["inflight"] -= need
                    state["done"] += need
                with open(bitmap, "a") as f:
                    f.write("{}\n".format(i))

    threads = [threading.Thread(target=worker, daemon=True) for _ in range(min(CONNS, max(1, nchunks)))]
    for t in threads:
        t.start()

    def report():
        while any(t.is_alive() for t in threads):
            time.sleep(1)
            with lock:
                n = state["done"] + state["inflight"]
            print("DL_PROGRESS {} {}".format(n, total), flush=True)
    mon = threading.Thread(target=report, daemon=True)
    mon.start()
    for t in threads:
        t.join()
    mon.join(2)
    os.close(fd)
    if errors:
        raise RuntimeError("; ".join(errors[:3]))
    if os.path.getsize(out_path) != total:
        raise RuntimeError("size mismatch")
    print("DL_PROGRESS {} {}".format(total, total), flush=True)


class Fatal(Exception):
    pass


out = None
try:
    total, etag = total_size(URL)
    out = os.path.join(TMP, os.path.basename(FNAME))
    fetch_range(URL, total, out)
    if EXPECT_SHA and os.environ.get("KIT_VERIFY_SHA") == "1":
        if sha256_of(out).lower() != EXPECT_SHA.lower():
            os.remove(out)
            raise Fatal("sha256 mismatch")  # corrupt transfer: retrying the same way will not help
except Fatal:
    raise
except Exception as e:
    print("  range download failed ({!r}); falling back to huggingface_hub".format(e), flush=True)
    os.environ["HF_HUB_ENABLE_HF_TRANSFER"] = "1"
    os.environ.setdefault("HF_HUB_DISABLE_XET", "1")
    try:
        from huggingface_hub import hf_hub_download
    except ImportError:
        subprocess.run([sys.executable, "-m", "pip", "install", "-q", "huggingface_hub"], check=False)
        from huggingface_hub import hf_hub_download
    out = hf_hub_download(REPO, FNAME, revision=REV or "main", local_dir=TMP, token=TOKEN or None)
print(out, flush=True)
"""
HF_DL_SCRIPT = DL_SCRIPT
HF_TMP = "/content/hf"
STORE_ROOT = None   # set to a RAM tmpfs root when the RAM model store is active
STALL_S = int(os.environ.get("KIT_STALL_S", "30"))
RETRYABLE = re.compile(r"429|Too Many|\b5\d\d\b|timed out|Timeout|Connection|reset by peer")


def mem_available():
    """Bytes of RAM the kernel can hand out right now."""
    try:
        with open("/proc/meminfo") as f:
            for line in f:
                if line.startswith("MemAvailable:"):
                    return int(line.split()[1]) * 1024
    except OSError:
        pass
    return 0


def setup_model_store(plan):
    """Optionally keep the model files in a RAM-backed tmpfs instead of on the VM disk.

    Measured on the Colab L4: sequential disk writes top out at ~292 MB/s while /dev/shm does
    ~2.4 GB/s, so a RAM store turns the download stage from disk-bound into network-bound
    (the 24.9 GB default profile lands ~45 s instead of ~68 s) and lets ComfyUI boot faster.
    It costs ~25 GB of RAM, so it is opt-in and falls back to disk whenever RAM is too tight.
    """
    mode = os.environ.get("KIT_MODEL_STORE", PLAN.get("model_store", "disk"))
    if mode != "ram":
        return None
    total = sum(int((m.get("size_gb") or 0) * 1e9) for m in plan["models"])
    avail = mem_available()
    need = int(total * 1.15) + (4 * 1000 ** 3)  # models + slack for torch/offload
    if avail < need:
        print(f"  model store: RAM {avail / 1e9:.0f} GB free < {need / 1e9:.0f} GB needed -> disk", flush=True)
        return None
    root = "/mnt/ram"
    os.makedirs(root, exist_ok=True)
    size_mb = int((total * 1.1 + 2 * 1000 ** 3) // (1024 * 1024))
    if root not in open("/proc/mounts").read():
        rc = sh(f"mount -t tmpfs -o size={size_mb}M,mode=0755 tmpfs {root}", check=False, quiet=True)
        if rc:
            print("  model store: tmpfs mount failed -> disk", flush=True)
            return None
    os.makedirs(f"{root}/models", exist_ok=True)
    print(f"  model store: RAM tmpfs at {root} ({total / 1e9:.1f} GB models, {size_mb / 1024:.0f} GB tmpfs)", flush=True)
    return root


def point_models_to(root):
    """Stage downloads on the RAM filesystem. Model files are placed in the RAM store and exposed to
    ComfyUI as symlinks, so an on-demand workflow that no longer fits in RAM simply lands on disk
    instead of failing with ENOSPC."""
    global HF_TMP, STORE_ROOT
    os.makedirs(f"{root}/models", exist_ok=True)
    HF_TMP = f"{root}/hf"  # same filesystem -> the final move is a rename, not a copy
    STORE_ROOT = root
    os.makedirs(HF_TMP, exist_ok=True)


def du(path):
    total = 0
    for root, _, files in os.walk(path):
        for f in files:
            try:
                total += os.lstat(os.path.join(root, f)).st_size
            except OSError:
                pass
    return total


def comfy_busy():
    """True while ComfyUI is executing a prompt (background downloads pause)."""
    try:
        q = json.loads(urllib.request.urlopen(f"http://127.0.0.1:{PORT}/queue", timeout=3).read())
        return bool(q.get("queue_running"))
    except Exception:
        return False


class Downloads:
    def __init__(self, models, concurrency, busy=comfy_busy, stall_s=STALL_S):
        self.models = models
        self.size = {m["id"]: int((m.get("size_gb") or 0) * 1e9) for m in models}
        self.total = sum(self.size.values())
        self.core_ids = [m["id"] for m in models if m.get("core")]
        self.core_total = sum(self.size[i] for i in self.core_ids)
        self.done_bytes, self.core_done_bytes = 0, 0
        self.files_done = 0
        self.errors = []
        self.restarts = 0
        self.lock = threading.Lock()
        self.live = {}   # id -> tmp dir of an in-flight HF download
        self.child_bytes = {}  # id -> bytes the downloader child reports (accurate with sparse writes)
        self.procs = {}  # id -> (Popen, pausable)
        self.paused = False
        self.pending_links = []  # (target, dst) pairs deferred until COMFY directory exists
        self.busy, self.stall_s = busy, stall_s
        self.pool = ThreadPoolExecutor(max_workers=max(1, concurrency))
        self.bg_pool = ThreadPoolExecutor(max_workers=max(1, concurrency))
        self.core_done = threading.Event()
        self.futures = {}
        self.t0 = time.time()

    def start(self):
        # core first, biggest first: the critical path is the largest core file
        order = sorted(self.models, key=lambda m: (not m.get("core"), -(m.get("size_gb") or 0)))
        seen_sha = {}
        for m in order:
            sha = m.get("sha256")
            if sha and sha in seen_sha:
                # identical file under a second name: link after the first one lands
                self.futures[m["id"]] = self.bg_pool.submit(self._link_after, seen_sha[sha], m)
                continue
            if sha:
                seen_sha[sha] = m["id"]
            if m.get("core"):
                self.futures[m["id"]] = self.pool.submit(self._one, m)
            else:
                # non-core files must not steal bandwidth from the critical path: they start once core is done
                self.futures[m["id"]] = self.bg_pool.submit(self._after_core, m)
        core = [self.futures[i] for i in self.core_ids if i in self.futures]
        threading.Thread(target=lambda: ([f.exception() for f in core], self.core_done.set()), daemon=True).start()
        threading.Thread(target=self._report, daemon=True).start()
        threading.Thread(target=self._pauser, daemon=True).start()

    def all_done(self):
        return self.files_done >= len(self.models) or bool(self.errors)

    def _after_core(self, m):
        self.core_done.wait()
        while self.paused:
            time.sleep(1)
        return self._one(m)

    def _link_after(self, first_id, m):
        self.futures[first_id].result()
        src = next(x for x in self.models if x["id"] == first_id)
        src_target = self._ram_target(src) or f"{COMFY}/models/{src['dest']}"
        dst = f"{COMFY}/models/{m['dest']}"
        if not os.path.isdir(COMFY):
            with self.lock:
                self.pending_links.append((src_target, dst))
        else:
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            if not (os.path.islink(dst) or os.path.exists(dst)):
                if os.path.exists(src_target):
                    os.symlink(src_target, dst)
                elif os.path.exists(f"{COMFY}/models/{src['dest']}"):
                    os.link(f"{COMFY}/models/{src['dest']}", dst)
        self._finish(m)

    def _pauser(self):
        while not self.all_done():
            time.sleep(2)
            want = self.busy()
            if want == self.paused:
                continue
            self.paused = want
            for p, pausable in list(self.procs.values()):
                if pausable and p.poll() is None:
                    try:
                        p.send_signal(signal.SIGSTOP if want else signal.SIGCONT)
                    except OSError:
                        pass
            print(f"  background downloads {'paused: prompt running' if want else 'resumed'}", flush=True)

    def _hf(self, m, repo, rev, fname, dst):
        tmp = f"{HF_TMP}/{m['id']}"
        pausable = not m.get("core")
        attempt = 0
        while True:
            p = subprocess.Popen([sys.executable, "-c", HF_DL_SCRIPT, repo, fname, rev, tmp, m.get("sha256") or ""],
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors="replace")
            with self.lock:
                self.live[m["id"]] = tmp
                self.procs[m["id"]] = (p, pausable)
            if pausable and self.paused:
                p.send_signal(signal.SIGSTOP)
            out = []

            def _pump():
                for line in p.stdout:
                    if line.startswith("DL_PROGRESS "):
                        try:
                            n = int(line.split()[1])
                            with self.lock:
                                self.child_bytes[m["id"]] = n
                        except (IndexError, ValueError):
                            pass
                    else:
                        out.append(line)
            reader = threading.Thread(target=_pump, daemon=True)
            reader.start()
            last, last_t, stalled = -1, time.time(), False
            while p.poll() is None:
                time.sleep(3)
                if pausable and self.paused:
                    last_t = time.time()  # paused time is not a stall
                    continue
                with self.lock:
                    reported = self.child_bytes.get(m["id"])
                size = reported if reported is not None else du(tmp)
                target = int(m.get("size_gb", 0) * 1e9)
                is_verifying = target > 0 and size >= int(target * 0.98)
                max_stall = (self.stall_s * 3) if is_verifying else self.stall_s
                if size != last:
                    last, last_t = size, time.time()
                elif time.time() - last_t > max_stall:
                    print(f"  stall: no bytes for {max_stall}s on {m['id']}; restarting that file", flush=True)
                    p.kill()
                    stalled = True
                    break
            p.wait()
            reader.join(5)
            with self.lock:
                self.procs.pop(m["id"], None)
            text = "".join(out).strip()
            if p.returncode == 0 and not stalled and text:
                path = text.splitlines()[-1].strip()
                break
            attempt += 1
            if stalled:
                self.restarts += 1
            if attempt >= 8 or (not stalled and not RETRYABLE.search(text)):
                raise RuntimeError(text[-300:] or f"exit {p.returncode}")
            wait = 2 if stalled else min(120, 10 * 2 ** (attempt - 1))
            if not stalled:
                print(f"  retry {m['id']} in {wait}s ({text[-120:]})", flush=True)
            time.sleep(wait)
        shutil.move(path, dst)  # local_dir keeps repo subfolders; the file always lands flat at models/<dest>
        with self.lock:
            self.live[m["id"]] = dst
        shutil.rmtree(tmp, ignore_errors=True)

    def _ram_target(self, m):
        """Where this file should really live: the RAM store when it fits, the disk otherwise."""
        if not STORE_ROOT:
            return None
        try:
            free = shutil.disk_usage(STORE_ROOT).free
        except OSError:
            return None
        need = int((m.get("size_gb") or 0) * 1e9 * 1.15) + 1 * 1000 ** 3
        if free > need:
            return f"{STORE_ROOT}/models/{m['dest']}"
        return None

    def _link(self, target, dst):
        if not os.path.isdir(COMFY):
            with self.lock:
                self.pending_links.append((target, dst))
            return
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        if os.path.islink(dst) or os.path.exists(dst):
            os.remove(dst) if os.path.islink(dst) else os.remove(dst)
        os.symlink(target, dst)

    def sync_links(self):
        """Link any downloads that finished before ComfyUI was cloned."""
        with self.lock:
            links = list(self.pending_links)
            self.pending_links.clear()
        for target, dst in links:
            try:
                os.makedirs(os.path.dirname(dst), exist_ok=True)
                if os.path.islink(dst) or os.path.exists(dst):
                    os.remove(dst) if os.path.islink(dst) else os.remove(dst)
                os.symlink(target, dst)
            except Exception:
                pass

    def _one(self, m):
        dst = f"{COMFY}/models/{m['dest']}"
        target = self._ram_target(m) or dst
        os.makedirs(os.path.dirname(target), exist_ok=True)
        if os.path.exists(target):
            if target != dst:
                self._link(target, dst)
            return self._finish(m)
        hm = HF_RE.match(m["url"])
        try:
            if hm:
                self._hf(m, hm.group(1), hm.group(2), hm.group(3), target)
            else:
                rc = sh(f"aria2c -q -x16 -s16 -k1M -d '{os.path.dirname(target)}' -o '{os.path.basename(target)}' '{m['url']}'",
                        check=False, quiet=True) if shutil.which("aria2c") else 1
                if rc:
                    with urllib.request.urlopen(m["url"], timeout=60) as r, open(target + ".part", "wb") as f:
                        shutil.copyfileobj(r, f, 8 << 20)
                    os.replace(target + ".part", target)
                if m.get("sha256"):
                    out = subprocess.run(["sha256sum", target], capture_output=True, text=True).stdout.split()[0]
                    if out.lower() != m["sha256"].lower():
                        os.remove(target)
                        raise RuntimeError(f"sha256 mismatch for {m['dest']}")
        except Exception as e:
            with self.lock:
                self.errors.append(f"{m['id']}: {e}")
            print(f"download failed {m['id']}: {e}", flush=True)
            raise
        if target != dst:
            self._link(target, dst)
        self._finish(m)

    def _finish(self, m):
        with self.lock:
            self.live.pop(m["id"], None)
            self.child_bytes.pop(m["id"], None)
            self.files_done += 1
            self.done_bytes += self.size[m["id"]]
            if m.get("core"):
                self.core_done_bytes += self.size[m["id"]]
        print(f"  ok {m['dest']}", flush=True)

    def snapshot(self):
        with self.lock:
            live = dict(self.live)
            done, core_done = self.done_bytes, self.core_done_bytes
        inflight = {}
        for i, t in live.items():
            reported = self.child_bytes.get(i)
            n = reported if reported is not None else du(t)
            inflight[i] = min(n, self.size[i] or 1 << 62)
        cur_b = done + sum(inflight.values())
        cur_cb = core_done + sum(v for i, v in inflight.items() if i in self.core_ids)
        with self.lock:
            if not hasattr(self, "_max_b"):
                self._max_b, self._max_cb = 0, 0
            self._max_b = max(self._max_b, cur_b)
            self._max_cb = max(self._max_cb, cur_cb)
            return (self._max_b, self._max_cb)

    def progress(self, rate, eta=None):
        b, cb = self.snapshot()
        left = max(0, self.total - b)
        if eta is None and rate > 1e5 and not self.paused:
            eta = int(left / rate)
        return {
            "bytes_done": b, "bytes_total": self.total,
            "core_bytes_done": cb, "core_bytes_total": self.core_total,
            "bytes_per_s": int(rate), "eta_s": eta,
            "files_done": self.files_done, "files_total": len(self.models),
            "core_done": self.core_done.is_set(), "paused": self.paused, "restarts": self.restarts,
            # legacy, file-granular
            "done_gb": round(b / 1e9, 2), "total_gb": round(self.total / 1e9, 2),
        }

    def _report(self):
        last_b, last_t, rate = self.snapshot()[0], time.time(), 0.0
        while not self.all_done():
            time.sleep(5)
            b, now = self.snapshot()[0], time.time()
            inst = max(0.0, (b - last_b) / max(1e-6, now - last_t))
            rate = inst if rate == 0 else 0.6 * rate + 0.4 * inst  # smoothed for a steady ETA
            last_b, last_t = b, now
            avg_rate = b / max(1.0, now - self.t0)
            effective_eta_rate = max(rate, 0.4 * avg_rate)
            left = max(0, self.total - b)
            eta = int(left / effective_eta_rate) if effective_eta_rate > 1e5 and not self.paused else None
            print("KIT_PROGRESS download " + json.dumps(self.progress(rate, eta=eta)), flush=True)
        print("KIT_PROGRESS download " + json.dumps(self.progress(0, eta=0)), flush=True)

    def wait(self, ids):
        for i in ids:
            self.futures[i].result()
        if set(self.core_ids).issubset(set(ids)):
            self.core_done.wait()


# ─────────────────────────── workflows ───────────────────────────
def png_workflow(data):
    b, i = data[8:], 0
    while i < len(b):
        n, = struct.unpack(">I", b[i:i + 4])
        t, d = b[i + 4:i + 8], b[i + 8:i + 8 + n]
        i += 12 + n
        if t == b"tEXt" and d.startswith(b"workflow\0"):
            return json.loads(d.split(b"\0", 1)[1])
    raise RuntimeError("PNG has no embedded workflow")


def remap_models(obj, aliases):
    """Profile `model_aliases`: rewrite loader filenames anywhere in a UI graph (nodes, subgraph definitions) or an
    API prompt (inputs), so every workflow uses the profile's shared weights. Returns the number of rewrites."""
    if not aliases:
        return 0
    n = 0
    if isinstance(obj, dict):
        for k, v in obj.items():
            if isinstance(v, str) and v in aliases:
                obj[k] = aliases[v]
                n += 1
            else:
                n += remap_models(v, aliases)
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            if isinstance(v, str) and v in aliases:
                obj[i] = aliases[v]
                n += 1
            else:
                n += remap_models(v, aliases)
    return n


def _slot_type_for(wname):
    if wname in ("text", "value", "prompt"):
        return "STRING"
    if wname in ("seed", "noise_seed", "width", "height", "batch_size"):
        return "INT"
    if wname == "image":
        return "IMAGE"
    if wname == "switch":
        return "BOOLEAN"
    if wname == "denoise":
        return "FLOAT"
    if wname == "choice":
        return "COMBO"
    return "*"


def decorate_app_nodes(g, wf):
    """Ensure nodes and widgets exposed in App Mode have clean, descriptive titles and labels."""
    nodes = {n["id"]: n for n in g.get("nodes", [])}
    for item in wf.get("app_inputs", []):
        nid, wname = item[0], item[1]
        custom_label = item[2] if len(item) > 2 and item[2] else None
        node = nodes.get(nid)
        if not node:
            continue
        ntype = str(node.get("type", ""))
        curr_title = str(node.get("title") or "")
        curr_lower = curr_title.lower()

        label = custom_label
        new_title = None

        if not label:
            if wname == "image":
                if "1" in curr_lower or "first" in curr_lower:
                    label = "首张参考底图 (First Image)"
                    new_title = "参考底图 (Input Image)"
                elif "2" in curr_lower or "second" in curr_lower or nid in (19, 475):
                    label = "第二张参考底图 (Second Image)"
                    new_title = "参考底图 (Input Image)"
                else:
                    label = "参考底图 (Input Image)"
                    new_title = "参考底图 (Input Image)"
            elif wname == "denoise":
                label = "重绘幅度 (Denoise)"
                new_title = "重绘幅度 (Denoise)"
            elif wname == "choice":
                label = "画质与速度预设 (Preset)"
                new_title = "画质与速度预设 (Preset)"
            elif wname in ("seed", "noise_seed"):
                label = "随机种子 (Seed)"
                new_title = "随机种子 (Seed)"
            elif wname == "width":
                label = "图像宽度 (Width)"
                new_title = "图像尺寸 (Image Size)"
            elif wname == "height":
                label = "图像高度 (Height)"
                new_title = "图像尺寸 (Image Size)"
            elif wname == "batch_size":
                label = "生成张数 (Batch Size)"
                new_title = "图像尺寸 (Image Size)"
            elif wname == "switch":
                label = "提示词增强 (Prompt Enhancer)"
                new_title = "提示词增强 (Prompt Enhancer)"
            elif wname in ("value", "text", "prompt"):
                if "width" in curr_lower:
                    label = "图像宽度 (Width)"
                    new_title = "图像宽度 (Width)"
                elif "height" in curr_lower:
                    label = "图像高度 (Height)"
                    new_title = "图像高度 (Height)"
                elif "batch" in curr_lower:
                    label = "生成张数 (Batch Size)"
                    new_title = "生成张数 (Batch Size)"
                elif "seed" in curr_lower or ntype == "PrimitiveInt":
                    label = "随机种子 (Seed)"
                    new_title = "随机种子 (Seed)"
                elif "neg" in curr_lower or nid in (4, 8, 40) or (nid == 18 and wname == "text") or (nid == 10 and "chinoll" in wf.get("id", "")):
                    label = "负面提示词 (Negative Prompt)"
                    new_title = "负面提示词 (Negative Prompt)"
                else:
                    label = "正向提示词 (Positive Prompt)"
                    new_title = "正向提示词 (Positive Prompt)"

        generic_titles = {"", "none", "cliptextencode", "primitivestringmultiline", "primitiveint",
                          "emptyflux2latentimage", "randomnoise", "basicscheduler", "loadimage", "customcombo"}
        if new_title and (not curr_title or curr_lower in generic_titles):
            node["title"] = new_title

        # Set widget label via LiteGraph's canonical backing slot channel:
        # LiteGraph configure() maps:
        #   const input = this.inputs.find((i) => i.widget?.name === w.name);
        #   if (input?.label) w.label = input.label;
        # CRITICAL:
        # 1. NEVER inject a 'widgets' key on node dictionary (destroys live widget class instances).
        # 2. Backing slot in node['inputs'] must carry {"widget": {"name": wname}, "label": label}.
        if label:
            inputs = node.setdefault("inputs", [])
            matched = False
            for slot in inputs:
                if slot.get("widget", {}).get("name") == wname or (slot.get("name") == wname and not slot.get("link")):
                    slot["label"] = label
                    slot.setdefault("widget", {})["name"] = wname
                    matched = True
                    break
            if not matched:
                inputs.append({
                    "name": wname,
                    "type": _slot_type_for(wname),
                    "widget": {"name": wname},
                    "label": label,
                    "link": None,
                })


def prepare_workflow(wf, data, default_view):
    """Apply declared filename overrides and ComfyUI App-mode metadata; graph otherwise untouched."""
    g = png_workflow(data) if wf["format"] == "png" else json.loads(data)
    nodes = {n["id"]: n for n in g.get("nodes", [])}
    for o in wf.get("overrides", []):
        n = nodes.get(o["node"])
        if not n or len(n.get("widgets_values") or []) <= o["widget_index"]:
            raise RuntimeError(f"{wf['id']}: override target {o} not found (upstream workflow changed?)")
        n["widgets_values"][o["widget_index"]] = o["value"]
    remap_models(g, PLAN.get("model_aliases") or {})
    decorate_app_nodes(g, wf)
    extra = g.setdefault("extra", {})
    if wf.get("app_outputs"):
        extra["linearData"] = {"inputs": [list(x[:2]) for x in wf.get("app_inputs", [])],
                               "outputs": list(wf["app_outputs"])}
        extra["linearMode"] = default_view == "app"
    return g


def install_workflows(update_default=True):
    d = f"{COMFY}/user/default/workflows"
    os.makedirs(d, exist_ok=True)
    default_file = None
    for wf in PLAN["workflows"]:
        data = wf["inline"].encode() if wf.get("inline") else fetch(wf["url"])
        g = prepare_workflow(wf, data, PLAN.get("default_view", "app"))
        for u in wf.get("loras") or []:
            dst = f"{COMFY}/models/loras/{u.rsplit('/', 1)[1]}"
            if not os.path.exists(dst):
                with open(dst + ".part", "wb") as f:
                    f.write(fetch(u, timeout=600))
                os.replace(dst + ".part", dst)
        name = f"{wf['id']}.json"
        json.dump(g, open(f"{d}/{name}", "w"), ensure_ascii=False)
        if wf.get("default"):
            default_file = name
        print(f"  workflow {name} ({wf['title']})", flush=True)
    if update_default:
        workflows_summary = [
            {"id": w["id"], "title": w.get("title", w["id"]), "file": f"{w['id']}.json"}
            for w in PLAN.get("workflows", [])
        ]
        json.dump({"default_workflow": default_file, "default_view": PLAN.get("default_view", "app"),
                   "workflows": workflows_summary},
                  open(f"{COMFY}/user/default/kit.json", "w"))
        if default_file and os.path.exists(f"{d}/{default_file}"):
            shutil.copy(f"{d}/{default_file}", f"{COMFY}/default_workflow.json")


def apply_settings():
    p = f"{COMFY}/user/default/comfy.settings.json"
    os.makedirs(os.path.dirname(p), exist_ok=True)
    cur = json.load(open(p)) if os.path.exists(p) else {}
    cur.update(PLAN["comfyui"].get("settings") or {})
    json.dump(cur, open(p, "w"), indent=1)


HEARTBEAT_NODE = '''
import json, os, time
from aiohttp import web
from server import PromptServer
HB = "/content/kit_heartbeat"
KIT = "/content/ComfyUI/user/default/kit.json"
COMFY = "/content/ComfyUI"

@PromptServer.instance.routes.post("/kit/heartbeat")
@PromptServer.instance.routes.get("/kit/heartbeat")
async def _hb(request):
    try:
        open(HB, "w").write(str(time.time()))
    except Exception:
        pass
    return web.json_response({"ok": True})

@web.middleware
async def _touch_hb_mw(request, handler):
    try:
        open(HB, "w").write(str(time.time()))
    except Exception:
        pass
    return await handler(request)

try:
    if hasattr(PromptServer.instance, "app") and _touch_hb_mw not in PromptServer.instance.app.middlewares:
        PromptServer.instance.app.middlewares.append(_touch_hb_mw)
except Exception:
    pass

@PromptServer.instance.routes.get("/kit/info")
async def _info(request):
    return web.json_response(json.load(open(KIT)) if os.path.exists(KIT) else {})

@PromptServer.instance.routes.get("/kit/workflow")
async def _default_wf(request):
    p = f"{COMFY}/default_workflow.json"
    if os.path.exists(p):
        return web.FileResponse(p, headers={"Content-Type": "application/json", "Cache-Control": "no-cache"})
    return web.Response(status=404, text="default workflow not found")

@PromptServer.instance.routes.get("/kit/workflows/{name:.*}")
async def _named_wf(request):
    name = request.match_info["name"]
    p = f"{COMFY}/user/default/workflows/{name}"
    if os.path.exists(p):
        return web.FileResponse(p, headers={"Content-Type": "application/json", "Cache-Control": "no-cache"})
    return web.Response(status=404, text="workflow not found")

@PromptServer.instance.routes.get("/api/userdata/workflows/{name:.*}")
@PromptServer.instance.routes.get("/userdata/workflows/{name:.*}")
async def _subpath_wf(request):
    name = request.match_info["name"]
    p = f"{COMFY}/user/default/workflows/{name}"
    if os.path.exists(p):
        return web.FileResponse(p, headers={"Content-Type": "application/json", "Cache-Control": "no-cache"})
    return web.Response(status=404, text="workflow not found")

NODE_CLASS_MAPPINGS = {}
WEB_DIRECTORY = "./web"
'''

KIT_JS = r'''
import { app } from "../../scripts/app.js";

app.registerExtension({
  name: "kit.init",
  async setup() {
    // Defer execution so ComfyUI can finish constructing LGraphCanvas and mounting the DOM
    setTimeout(async () => {
      try {
        const info = await (await fetch("/kit/info")).json();
        if (!info.default_workflow) return;
        if (sessionStorage.getItem("kit.loaded_wf") === info.default_workflow) return;

        // Poll until canvas and graph are fully initialized by ComfyUI
        for (let i = 0; i < 60; i++) {
          if (app.canvas && app.graph && (app.canvas.canvas || document.querySelector("canvas"))) break;
          await new Promise(r => setTimeout(r, 100));
        }
        if (!app.canvas || !app.graph) return;

        let res = await fetch("/kit/workflow");
        if (!res.ok) res = await fetch("/kit/workflows/" + encodeURIComponent(info.default_workflow));
        if (!res.ok) res = await fetch("/default_workflow");
        if (!res.ok) res = await fetch("/api/userdata/workflows/" + encodeURIComponent(info.default_workflow));
        if (res.ok) {
          const wf = await res.json();
          await app.loadGraphData(wf, true, true, info.default_workflow);
          sessionStorage.setItem("kit.loaded_wf", info.default_workflow);
          if (info.default_view === "app") {
            setTimeout(async () => {
              try {
                const isLinear = !!document.querySelector('[data-testid="linear-mode"], .linear-mode, [class*="linear"]');
                if (!isLinear) {
                  await app.extensionManager.command.execute("Comfy.ToggleLinear");
                }
              } catch (e) {}
            }, 300);
          }
        }
      } catch (e) {
        console.warn("[kit.init]", e);
      }
    }, 600);
  }
});
'''


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

def _long_slot_capacity(slots):
    # b12211 pads context to multiples of 256 (1,000,000 becomes 1,000,192).
    # Keep a single real slot and the advertised 1M minimum, not exact equality.
    if not isinstance(slots, list) or len(slots) != 1 or not isinstance(slots[0], dict):
        return 0
    capacity = slots[0].get("n_ctx")
    return capacity if type(capacity) is int and capacity >= 1000000 else 0

# Restricted text-only llama.cpp adapter; normal Ollama proxy remains unchanged.
LLAMA = "http://127.0.0.1:11435"
LONG_OUTPUT = 16384

def _long_info():
    try:
        with open("/content/wb_ai.json") as f:
            return json.load(f)
    except Exception:
        return {}

def _long_messages(body):
    messages = body.get("messages") or []
    if not isinstance(messages, list) or not messages:
        raise ValueError("messages required")
    out = []
    for message in messages:
        if message.get("images"):
            raise ValueError("1M experimental profile is text-only; use the vision profile for images")
        if message.get("role") not in ("system", "user", "assistant") or not isinstance(message.get("content"), str):
            raise ValueError("invalid text message")
        out.append({"role": message["role"], "content": message["content"]})
    return out

async def _long_post(session, path, body):
    async with session.post(LLAMA + path, json=body) as response:
        if response.status != 200:
            raise RuntimeError(f"1M service {path}: HTTP {response.status}: " + (await response.text())[:500])
        return await response.json()

async def _long_count(session, messages, think):
    rendered = await _long_post(session, "/apply-template", {
        "messages": messages, "chat_template_kwargs": {"enable_thinking": think}})
    if not isinstance(rendered.get("prompt"), str):
        raise RuntimeError("1M template service returned no prompt")
    encoded = await _long_post(session, "/tokenize", {"content": rendered["prompt"], "add_special": True, "parse_special": True})
    if not isinstance(encoded.get("tokens"), list):
        raise RuntimeError("1M tokenizer returned no token count")
    return len(encoded["tokens"])

async def _wb_long_proxy(request, tail):
    import asyncio
    info = _long_info()
    name = (info.get("llm") or {}).get("name")
    if tail == "api/show":
        return web.json_response({"capabilities": ["completion"], "model_info": {"qwen35.context_length": 1000000}})
    if tail not in ("api/chat", "api/context-check") or request.method != "POST":
        return web.json_response({"error": "not allowed in 1M profile"}, status=404)
    try:
        body = await request.json()
        if body.get("model") != name or not info.get("context_verified"):
            raise ValueError("1M runtime/model is not verified; wake the 1M profile again")
        messages = _long_messages(body)
        think = bool(body.get("think", False))
        opts = body.get("options") or {}
        if int(opts.get("num_ctx", 1000000)) != 1000000:
            raise ValueError("1M profile requires num_ctx=1000000")
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=None, sock_connect=5, sock_read=3600)) as session:
            # Verify live allocation as well as the startup report (no stale 1M label).
            async with session.get(LLAMA + "/slots") as response:
                slots = await response.json()
            if not _long_slot_capacity(slots):
                raise RuntimeError("1M slot allocation is no longer available")
            count = await _long_count(session, messages, think)
            budget = 1000000 - LONG_OUTPUT - 4096
            if tail == "api/context-check":
                return web.json_response({"tokens": count, "input_budget": budget, "context_tokens": 1000000})
            if count > budget:
                return web.json_response({"error": f"输入 {count} tokens，超过本次预算 {budget}；请缩短文字或附件。"}, status=400)
            payload = {"model": name, "messages": messages, "stream": True,
                       "max_tokens": LONG_OUTPUT, "temperature": opts.get("temperature", 0.7),
                       "top_p": 0.95 if think else 0.8, "top_k": 20, "min_p": 0.0,
                       "presence_penalty": 0.0 if think else 1.5, "repeat_penalty": 1.0,
                       "chat_template_kwargs": {"enable_thinking": think},
                       "stream_options": {"include_usage": True}}
            if body.get("format") == "json":
                payload["response_format"] = {"type": "json_object"}
            out = web.StreamResponse(headers={"Content-Type": "application/x-ndjson", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"})
            await out.prepare(request)
            # Send headers before prefill; keep the proxy connection alive while the
            # GPU processes a large prompt. Empty JSON is ignored by existing clients.
            owner = asyncio.current_task()
            async def heartbeat():
                try:
                    while True:
                        await asyncio.sleep(15)
                        await out.write(b"{}\n")
                except (ConnectionResetError, RuntimeError):
                    owner.cancel()
            pulse = asyncio.create_task(heartbeat())
            try:
                await out.write((json.dumps({"wb_tokens": {"input": count, "window": 1000000, "output_reserved": LONG_OUTPUT}}) + "\n").encode())
                async with session.post(LLAMA + "/v1/chat/completions", json=payload) as response:
                    if response.status != 200:
                        raise RuntimeError(f"1M inference HTTP {response.status}: " + (await response.text())[:1000])
                    usage = {}
                    ended = False
                    async for line in response.content:
                        if not line.startswith(b"data: "):
                            continue
                        value = line[6:].strip()
                        if value == b"[DONE]":
                            ended = True
                            break
                        event = json.loads(value)
                        if event.get("error"):
                            raise RuntimeError(str(event["error"])[:1000])
                        if event.get("usage"):
                            usage = event["usage"]
                        for choice in event.get("choices", []):
                            delta = choice.get("delta") or {}
                            if delta.get("content") or delta.get("reasoning_content"):
                                await out.write((json.dumps({"message": {"content": delta.get("content", ""), "thinking": delta.get("reasoning_content", "")}}) + "\n").encode())
                    if not ended:
                        raise RuntimeError("1M response stream ended early; see diagnostic logs")
                    await out.write((json.dumps({"done": True, "eval_count": usage.get("completion_tokens", 0)}) + "\n").encode())
            except (ConnectionResetError, asyncio.CancelledError):
                raise
            except Exception as e:
                await out.write((json.dumps({"error": str(e)[:1000]}) + "\n").encode())
            finally:
                pulse.cancel()
                try:
                    await pulse
                except asyncio.CancelledError:
                    pass
            await out.write_eof()
            return out
    except ValueError as e:
        return web.json_response({"error": str(e)}, status=400)
    except Exception as e:
        return web.json_response({"error": "1M 服务错误：" + str(e)[:1000]}, status=502)

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
            endpoint = LLAMA + "/health" if (info.get("llm") or {}).get("engine") == "llama_cpp_1m" else OLLAMA + "/api/version"
            async with s.get(endpoint) as r:
                up = r.status == 200
    except Exception:
        pass
    return web.json_response({**info, "llm_up": up})


@_routes.route("*", "/wbllm/{tail:.*}")
async def _wb_proxy(request):
    tail = request.match_info["tail"]
    if (_long_info().get("llm") or {}).get("engine") == "llama_cpp_1m":
        request._client_max_size = 32 << 20
        return await _wb_long_proxy(request, tail)
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


_WB_OLLAMA_MODELS = "/content/ollama_models"


def _ollama_prepare_storage():
    """Keep the original disk path unless importing would exhaust it. Called before serve.
    A RAM source plus a disk blob plus compat-migrate can otherwise require three copies.
    Only enlarge our existing tmpfs, leaving explicit headroom for the runtime."""
    global _WB_OLLAMA_MODELS
    total = sum(int((m.get("size_gb") or 0) * 1e9) for m in PLAN.get("models", []))
    if total <= 0:
        return
    disk = "/content/ollama_models"
    os.makedirs(disk, exist_ok=True)
    budget = int(total * 2.2) + 4_000_000_000
    disk_needed = int(total * 1.1) + 2_000_000_000  # source will be linked, only rewrite needs disk
    free = shutil.disk_usage(disk).free
    print(f"KIT_OLLAMA_STORAGE disk_free={free / 1e9:.1f}GB linked_import_budget={disk_needed / 1e9:.1f}GB", flush=True)
    if free >= disk_needed:
        print("KIT_OLLAMA_STORAGE keeping disk with verified source-link reuse; copy fallback requires a separate capacity check", flush=True)
        return
    root = globals().get("STORE_ROOT")
    # Do not turn a disk profile into a RAM profile, alter another mount, or move existing models.
    if root != "/mnt/ram" or not os.path.ismount(root):
        print(f"KIT_OLLAMA_STORAGE keeping disk; model_store={root!r}; no active managed RAM mount", flush=True)
        return
    if os.listdir(disk):
        print("KIT_OLLAMA_STORAGE existing model store retained", flush=True)
        return
    # Account for RAM already occupied by this tmpfs while downloads overlap installation.
    available = mem_available()
    usage = shutil.disk_usage(root)
    required = max(0, budget - (usage.total - usage.free)) + 24_000_000_000
    if available < required:
        print(f"KIT_OLLAMA_STORAGE RAM reserve insufficient: {available / 1e9:.1f}GB < {required / 1e9:.1f}GB; keeping disk", flush=True)
        return
    size_mb = (budget + (1 << 20) - 1) // (1 << 20)
    current = usage.total
    size_mb = max(size_mb, (current + (1 << 20) - 1) // (1 << 20))
    try:
        rc = subprocess.run(["mount", "-o", f"remount,size={size_mb}M", root],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        print("KIT_OLLAMA_STORAGE tmpfs resize unavailable; keeping disk", flush=True)
        return
    if rc.returncode:
        print("KIT_OLLAMA_STORAGE tmpfs resize failed; keeping disk", flush=True)
        return
    candidate = os.path.join(root, "ollama_models")
    os.makedirs(candidate, exist_ok=True)
    if os.stat(candidate).st_dev != os.stat(os.path.join(root, "models")).st_dev:
        raise RuntimeError("Ollama RAM store is not on the model filesystem")
    _WB_OLLAMA_MODELS = candidate
    print(f"KIT_OLLAMA_STORAGE RAM selected; capacity={size_mb / 1024:.1f}GiB; source hard-link reuse and compatibility rewrite reserve", flush=True)


def _ollama_check_import_space(path, reused):
    """Reserve another full GGUF for Ollama's compatibility metadata rewrite."""
    blobs = os.path.join(_ollama_env()["OLLAMA_MODELS"], "blobs")
    os.makedirs(blobs, exist_ok=True)
    size = os.path.getsize(path)
    needed = int(size * (1.1 if reused else 2.1)) + 2_000_000_000
    free = shutil.disk_usage(blobs).free
    print(f"KIT_OLLAMA_STORAGE before_import free={free / 1e9:.1f}GB required={needed / 1e9:.1f}GB reused={reused}", flush=True)
    if free < needed:
        raise RuntimeError(f"Ollama import storage insufficient: {free / 1e9:.1f} GB free, {needed / 1e9:.1f} GB required including compatibility conversion. Use a VM with more disk/RAM or a smaller quantization; import was not started.")
    if _ollama_env()["OLLAMA_MODELS"].startswith("/mnt/ram/") and mem_available() < needed + 24_000_000_000:
        raise RuntimeError("Ollama import RAM reserve insufficient; import was not started. Use a VM with more RAM or a smaller quantization.")



def _long_slot_capacity(slots):
    # b12211 pads context to multiples of 256 (1,000,000 becomes 1,000,192).
    # Keep a single real slot and the advertised 1M minimum, not exact equality.
    if not isinstance(slots, list) or len(slots) != 1 or not isinstance(slots[0], dict):
        return 0
    capacity = slots[0].get("n_ctx")
    return capacity if type(capacity) is int and capacity >= 1000000 else 0

LLAMA_ROOT = "/content/wb_llama_b12211"
LLAMA_URL = "http://127.0.0.1:11435"
LLAMA_ASSETS = (
    ("llama-b12211-bin-ubuntu-cuda-12.8-x64.tar.gz", 188258232, 724566843,
     "103fac975e807da6cdb5717e87ca594e25bb7703dd8a7f10c22269c124e1a552"),
    ("cudart-llama-b12211-bin-ubuntu-cuda-12.8-x64.tar.gz", 594373633, 724560807,
     "d66dba34f0e45c66ce7d4484116c681aa71ff257ee1e47b02d9775aa59b5ef77"),
)

def llama_install():
    import hashlib
    import tarfile
    from pathlib import Path
    root = Path(LLAMA_ROOT)
    root.mkdir(parents=True, exist_ok=True)
    for name, size, asset_id, digest in LLAMA_ASSETS:
        archive = root / name
        last = None
        for attempt in range(1, 5):
            try:
                print(f"KIT_LONG_CONTEXT download {name} attempt {attempt}/4", flush=True)
                url = (f"https://api.github.com/repos/ggml-org/llama.cpp/releases/assets/{asset_id}" if attempt % 2 else
                       "https://github.com/ggml-org/llama.cpp/releases/download/b12211/" + name)
                req = urllib.request.Request(
                    url, headers={"User-Agent": "wakeloom-kit", "Accept": "application/octet-stream"})
                h, count, tick = hashlib.sha256(), 0, time.monotonic()
                with urllib.request.urlopen(req, timeout=60) as response, open(archive, "wb") as out:
                    while True:
                        chunk = response.read(4 << 20)
                        if not chunk:
                            break
                        count += len(chunk)
                        if count > size:
                            raise RuntimeError("llama.cpp archive exceeds expected size")
                        h.update(chunk)
                        out.write(chunk)
                        if time.monotonic() - tick >= 10:
                            print(f"KIT_LONG_CONTEXT download {count//1000000}/{size//1000000} MB", flush=True)
                            tick = time.monotonic()
                if count != size or h.hexdigest() != digest:
                    raise RuntimeError("llama.cpp archive size/SHA-256 mismatch")
                with tarfile.open(archive, "r:gz") as tf:
                    # Python's data filter validates paths and links within the install root.
                    tf.extractall(root, filter="data")
                archive.unlink()
                break
            except Exception as e:
                last = e
                archive.unlink(missing_ok=True)
                print(f"KIT_LONG_CONTEXT download failed: {type(e).__name__}: {e}", flush=True)
                if attempt == 4:
                    raise RuntimeError(f"llama.cpp install failed: {last}") from e
                time.sleep(attempt * 2)
    if not list(root.rglob("llama-server")):
        raise RuntimeError("verified archive did not contain llama-server")

def llama_command(binary, path, name):
    return [str(binary), "--model", path, "--alias", name,
            "--host", "127.0.0.1", "--port", "11435", "--ctx-size", "1000000",
            "--parallel", "1", "--n-gpu-layers", "999", "--flash-attn", "on",
            "--cache-type-k", "q8_0", "--cache-type-v", "q8_0",
            "--rope-scaling", "yarn", "--rope-scale", "4", "--yarn-orig-ctx", "262144",
            "--override-kv", "qwen35.context_length=int:1000000",
            "--batch-size", "2048", "--ubatch-size", "512", "--jinja",
            "--no-context-shift", "--fit", "off", "--slots"]

def llama_finish(llm):
    from pathlib import Path
    root = Path(LLAMA_ROOT)
    binary = next(root.rglob("llama-server"))
    binary.chmod(0o755)
    path = os.path.realpath(f"{COMFY}/models/{llm['dest']}")
    if not os.path.isfile(path):
        raise RuntimeError("1M model file missing")
    # No silent CPU fallback for a large 1M cache; normal profiles remain available.
    gpu = subprocess.check_output(["nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits"], text=True)
    if max(int(x.strip()) for x in gpu.splitlines() if x.strip()) < 80000:
        raise RuntimeError("1M experimental profile requires at least 80 GB GPU memory; choose the normal 32K profile")
    libdirs = sorted({str(p.parent) for p in root.rglob("*.so*")})
    env = {**os.environ, "LD_LIBRARY_PATH": ":".join(libdirs + [os.environ.get("LD_LIBRARY_PATH", "")])}
    for key in ("HF_TOKEN", "CF_TUNNEL_TOKEN"):
        env.pop(key, None)
    cmd = llama_command(binary, path, llm["name"])
    print("KIT_LONG_CONTEXT loading 1,000,000 tokens; YaRN x4; Q8 KV; one slot; experimental", flush=True)
    with open(f"{LOG}/llama.log", "a") as log:
        process = subprocess.Popen(cmd, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    for step in range(600):
        if process.poll() is not None:
            tail = Path(f"{LOG}/llama.log").read_text(errors="replace")[-6000:]
            raise RuntimeError("1M runtime exited during loading:\n" + tail)
        try:
            with urllib.request.urlopen(LLAMA_URL + "/slots", timeout=3) as response:
                slots = json.load(response)
        except Exception:
            if step % 5 == 0:
                print(f"KIT_LONG_CONTEXT loading: {step*2}s", flush=True)
            time.sleep(2)
            continue
        capacity = _long_slot_capacity(slots)
        observed = [slot.get("n_ctx") if isinstance(slot, dict) else None for slot in slots] if isinstance(slots, list) else None
        print(f"KIT_LONG_CONTEXT requested=1000000 slot_contexts={observed}", flush=True)
        if not capacity:
            process.terminate()
            raise RuntimeError(f"1M slot allocation mismatch: expected one slot >=1000000, received {observed}; refusing to report ready")
        break
    else:
        process.terminate()
        raise RuntimeError("1M runtime did not confirm a 1,000,000-token slot; see kit_logs/llama.log")
    # Short inference catches template/backend faults; not a 1M retrieval benchmark.
    req = urllib.request.Request(LLAMA_URL + "/v1/chat/completions", method="POST",
        data=json.dumps({"model": llm["name"], "messages": [{"role": "user", "content": "Reply OK"}],
                         "stream": False, "max_tokens": 16, "chat_template_kwargs": {"enable_thinking": False}}).encode(),
        headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=180) as response:
            result = json.load(response)
        if not result.get("choices") or not result["choices"][0].get("message", {}).get("content"):
            raise RuntimeError("1M short inference check returned no text")
    except Exception:
        process.terminate()
        raise
    with open(AI_INFO) as f:
        info = json.load(f)
    info.update(ready=True, context_verified=True, context_tokens=1000000, allocated_context_tokens=capacity, vision=False)
    with open(AI_INFO, "w") as f:
        json.dump(info, f)
    print(f"KIT_LONG_CONTEXT allocated={capacity} advertised=1000000; short inference passed; full-window quality untested", flush=True)

def _ollama_env():
    llm = (PLAN.get("ai") or {}).get("llm") or {}
    env = {**os.environ, "OLLAMA_HOST": "127.0.0.1:11434", "OLLAMA_MODELS": _WB_OLLAMA_MODELS,
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
            if (ai.get("llm") or {}).get("engine") == "llama_cpp_1m":
                llama_install()
                return
            if not _ollama_bin():
                ollama_install()
            _ollama_prepare_storage()
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
    """Reuse the completed GGUF instead of copying it into Ollama.
    The normal CLI still hashes the source, registers the model and chooses its original defaults.
    Across filesystems, a verified symlink keeps the completed VM model in its original store.
    The source must remain for the VM lifetime; never move/delete it after registration.
    If linking is unavailable, leave both files untouched and use the original import path."""
    import hashlib
    import errno

    blobs = os.path.join(_ollama_env()["OLLAMA_MODELS"], "blobs")
    linked = None
    try:
        os.makedirs(blobs, exist_ok=True)
        path = os.path.realpath(path)
        source = os.stat(path)
        if source.st_size <= 0:
            return False
        cross_fs = source.st_dev != os.stat(blobs).st_dev
        print(f"KIT_OLLAMA_STORAGE source_device={source.st_dev} blob_device={os.stat(blobs).st_dev} cross_filesystem={cross_fs}", flush=True)

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
        link_kind = "hardlink"
        if cross_fs:
            os.symlink(path, blob)
            link_kind = "symlink"
        else:
            try:
                os.link(path, blob)
            except OSError as link_error:
                if link_error.errno != errno.EXDEV:
                    raise
                # Bind/overlay mounts can report equal device IDs yet reject a hard link.
                os.symlink(path, blob)
                link_kind = "symlink"
        linked = blob
        if identity(os.stat(blob)) != identity(source):
            os.unlink(blob)
            return False
        print(f"  reusing {source.st_size / 1e9:.1f} GB model without an extra copy ({link_kind}); source retained", flush=True)
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
    _ollama_check_import_space(path, reused)
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
            msg = (f"  … {int(time.time() - t0)}s, reusing model file; preparing Ollama metadata" if reused else
                   f"  … {int(time.time() - t0)}s, import writes {done / 1e9:.1f} GB (source {size / 1e9:.1f} GB; includes compatibility conversion)")
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
            raise RuntimeError("1M runtime install timed out" if llm.get("engine") == "llama_cpp_1m" else "Ollama install timed out")
        if box.get("error"):
            raise box["error"]
        if llm.get("engine") == "llama_cpp_1m":
            llama_finish(llm)
            return
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


def install_custom_nodes():
    """Install the plan's custom nodes; returns the ids that were newly installed."""
    new = []
    base = f"{COMFY}/custom_nodes"
    os.makedirs(f"{base}/kit_heartbeat/web", exist_ok=True)
    open(f"{base}/kit_heartbeat/__init__.py", "w").write(HEARTBEAT_NODE)
    open(f"{base}/kit_heartbeat/web/kit.js", "w").write(KIT_JS)
    os.makedirs(f"{base}/wb_ai", exist_ok=True)
    open(f"{base}/wb_ai/__init__.py", "w").write(AI_NODE)
    for n in PLAN.get("custom_nodes", []):
        dst = f"{base}/{n['id']}"
        if os.path.isdir(dst):
            continue
        if n["url"].startswith("https://huggingface.co/"):
            repo = n["url"].split("huggingface.co/", 1)[1].strip("/")
            from huggingface_hub import snapshot_download
            tmp = snapshot_download(repo, allow_patterns=[f"{n['subdir']}/*"] if n.get("subdir") else None,
                                    revision=n.get("ref") or None, local_dir=f"/content/hf/node_{n['id']}")
            shutil.copytree(os.path.join(tmp, n.get("subdir") or ""), dst)
        else:
            sh(f"git clone -q {n['url']} {dst}" + (f" && cd {dst} && git checkout -q {n['ref']}" if n.get("ref") else ""))
            if n.get("subdir"):
                inner = f"{dst}.tmp"
                os.rename(dst, inner)
                shutil.copytree(f"{inner}/{n['subdir']}", dst)
                shutil.rmtree(inner)
        if os.path.exists(f"{dst}/requirements.txt"):
            sh(f"uv pip install --system -q -r {dst}/requirements.txt || pip install -q -r {dst}/requirements.txt", check=False)
        new.append(n["id"])
    return new


def start_quick_tunnel():
    """Local mode (ComfyWorkbench): no Cloudflare account. An auth proxy (/content/kit/wbproxy.py, checks
    the per-session X-WB-Key header) fronts ComfyUI, and a cloudflared quick tunnel exposes it on a random
    trycloudflare.com URL that is printed as KIT_TUNNEL and kept in /content/kit_tunnel_url."""
    penv = {**os.environ, "WB_PORT": "8189", "WB_UPSTREAM": f"127.0.0.1:{PORT}"}
    penv.pop("HF_TOKEN", None)
    subprocess.run(["bash", "-lc", "pkill -f wbproxy.py || true; pkill -f 'cloudflared tunnel' || true"], check=False)
    subprocess.Popen([sys.executable, "/content/kit/wbproxy.py"], env=penv, stdout=open(f"{LOG}/wbproxy.log", "w"),
                     stderr=subprocess.STDOUT, start_new_session=True)
    subprocess.Popen(["bash", "-lc", "nohup cloudflared tunnel --no-autoupdate --protocol http2 --url http://127.0.0.1:8189 "
                                     f"> {LOG}/cloudflared.log 2>&1 &"])
    for _ in range(90):
        try:
            m = re.search(r"https://[a-z0-9-]+\.trycloudflare\.com", open(f"{LOG}/cloudflared.log").read())
        except FileNotFoundError:
            m = None
        if m:
            open("/content/kit_tunnel_url", "w").write(m.group(0))
            print("KIT_TUNNEL " + json.dumps({"url": m.group(0)}), flush=True)
            return
        time.sleep(1)
    print("KIT_TUNNEL_FAILED quick tunnel did not report a URL", flush=True)


def start_tunnel():
    if os.environ.get("KIT_QUICK_TUNNEL"):
        return start_quick_tunnel()
    tok = os.environ.get("CF_TUNNEL_TOKEN", "").strip()
    sh("pkill -f 'cloudflared tunnel' || true", check=False, quiet=True)
    if not tok:
        print("WARNING: no CF_TUNNEL_TOKEN; ComfyUI is only reachable inside the VM", flush=True)
        return
    with open("/content/.cf_token", "w") as f:
        f.write(tok)
    os.chmod("/content/.cf_token", 0o600)
    subprocess.Popen(["bash", "-lc", f"nohup cloudflared tunnel --protocol http2 --no-autoupdate run --token-file /content/.cf_token "
                                     f"> {LOG}/cloudflared.log 2>&1 &"])


def comfy_cmd():
    args = ["python", "main.py", "--listen", "127.0.0.1", "--port", str(PORT), "--disable-auto-launch"]
    emp = PLAN["comfyui"].get("extra_model_paths")
    if emp:
        path = f"{COMFY}/extra_model_paths.kit.yaml"
        json.dump(emp, open(path, "w"))  # JSON is valid YAML
        args += ["--extra-model-paths-config", path]
    args += list(PLAN["comfyui"].get("args") or [])
    return args


def _is_comfy_busy() -> bool:
    try:
        import urllib.request
        with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/prompt", timeout=3) as r:
            data = json.loads(r.read())
            exec_info = data.get("exec_info", {})
            if exec_info.get("queue_remaining", 0) > 0:
                return True
    except Exception:
        pass
    return False


def spawn_watchdog():
    """Run the gate-loss watchdog in its own process.

    The watchdog has to outlive setup.py: setup.py returns as soon as ComfyUI is ready so the
    Jupyter kernel stays free for follow-up execs (the gate's heartbeat and the on-demand fetch
    both need the kernel). A separate process keeps the cost guard alive.
    """
    env = {**os.environ, "KIT_WATCHDOG_ONLY": "1", "KIT_T0": str(T0)}
    try:
        subprocess.Popen([sys.executable, os.path.abspath(__file__)], env=env,
                         stdout=open(f"{LOG}/watchdog.log", "w"), stderr=subprocess.STDOUT,
                         start_new_session=True)
    except Exception as e:  # noqa: BLE001
        print(f"[watchdog] spawn failed: {e!r}", flush=True)


def watchdog():
    lost = PLAN.get("watchdog_gate_lost_min", 20) * 60
    max_s = PLAN.get("max_session_hours", 7) * 3600
    t0 = float(os.environ.get("KIT_T0", T0))
    open(HB_FILE, "w").write(str(time.time()))
    consecutive_overdue = 0
    while True:
        time.sleep(60)
        # Revive cloudflared if process died
        if os.path.exists("/content/.cf_token") and os.system("pgrep -f 'cloudflared tunnel' > /dev/null") != 0:
            print("[watchdog] cloudflared died; restarting tunnel", flush=True)
            start_tunnel()
        if os.environ.get("KIT_QUICK_TUNNEL") and (os.system("pgrep -f 'cloudflared tunnel' > /dev/null") != 0
                                                   or os.system("pgrep -f wbproxy.py > /dev/null") != 0):
            print("[watchdog] quick tunnel died; restarting", flush=True)
            start_quick_tunnel()

        if (PLAN.get("ai") or {}).get("type") == "llm" and ((PLAN.get("ai") or {}).get("llm") or {}).get("engine") != "llama_cpp_1m" and _ollama_bin() and not _ollama_up():
            print("[watchdog] ollama died; restarting", flush=True)
            try:
                ollama_serve()
            except Exception as e:  # noqa: BLE001
                print(f"[watchdog] ollama restart failed: {e!r}", flush=True)
        if time.time() - t0 > max_s:
            print("[watchdog] session over limit; releasing VM", flush=True)
            break

        try:
            last = float(open(HB_FILE).read())
        except Exception:
            last = time.time()

        # Gate heartbeat check with two-phase verification (2 次检测)
        if time.time() - last > lost:
            # Check 1: If ComfyUI is generating images, do not release
            if _is_comfy_busy():
                print("[watchdog] gate heartbeat overdue but ComfyUI is generating; extending lease", flush=True)
                open(HB_FILE, "w").write(str(time.time()))
                consecutive_overdue = 0
                continue

            consecutive_overdue += 1
            print(f"[watchdog] gate heartbeat overdue check ({consecutive_overdue}/2)", flush=True)
            if consecutive_overdue < 2:
                continue

            print("[watchdog] gate lost confirmed after 2 consecutive checks; releasing VM", flush=True)
            break
        else:
            consecutive_overdue = 0

    subprocess.run(["bash", "-lc", "pkill -f cloudflared; pkill -f 'main.py --listen'"])
    try:
        from google.colab import runtime
        runtime.unassign()
    except Exception as e:
        open(f"{LOG}/watchdog.err", "w").write(repr(e))


def smoke_prompt(d):
    """Accept a bare API graph or a /prompt request body ({"prompt": {...}, ...}, as some repos ship it)."""
    if isinstance(d, dict) and isinstance(d.get("prompt"), dict):
        return d["prompt"]
    return d


def smoke():
    api = smoke_prompt(json.loads(fetch(PLAN["smoke"]["api_url"])))
    remap_models(api, PLAN.get("model_aliases") or {})
    req = urllib.request.Request(f"http://127.0.0.1:{PORT}/prompt", data=json.dumps({"prompt": api}).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        r = json.load(urllib.request.urlopen(req, timeout=30))
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"prompt rejected ({e.code}): {e.read().decode(errors='replace')[:600]}") from None
    if "prompt_id" not in r:
        raise RuntimeError(f"prompt rejected: {json.dumps(r)[:500]}")
    pid = r["prompt_id"]
    deadline = time.time() + PLAN["smoke"].get("timeout_s", 600)
    while time.time() < deadline:
        time.sleep(2)
        h = json.load(urllib.request.urlopen(f"http://127.0.0.1:{PORT}/history/{pid}", timeout=10))
        if pid in h:
            st = h[pid].get("status", {})
            if st.get("status_str") == "error":
                raise RuntimeError(json.dumps(st.get("messages"))[:800])
            if any(o.get("images") for o in h[pid].get("outputs", {}).values()):
                return
    raise RuntimeError("timeout waiting for smoke image")


def fetch_only():
    """On-demand download on a running VM (gate: POST /_gate/fetch). No KIT_STAGE lines: the session state
    stays `ready`; progress arrives as KIT_PROGRESS, the result as KIT_FETCHED / KIT_FETCH_FAILED."""
    os.environ["HF_HUB_DISABLE_PROGRESS_BARS"] = "1"
    os.environ["HF_HUB_ENABLE_HF_TRANSFER"] = "1"
    os.environ["HF_HUB_DISABLE_XET"] = "1"
    os.environ["KIT_DL_CONNS"] = str(PLAN.get("download_connections", 8))
    if PLAN.get("xet_high_performance", True):
        os.environ["HF_XET_HIGH_PERFORMANCE"] = "1"  # only bites on the huggingface_hub fallback path
    ids = [w["id"] for w in PLAN["workflows"]]
    try:
        store = setup_model_store(PLAN)
        if store and not os.path.islink(f"{COMFY}/models"):
            point_models_to(store)
        new_nodes = install_custom_nodes()
        install_workflows(update_default=False)
        dl = Downloads(PLAN["models"], PLAN.get("download_concurrency", 8))
        dl.start()
        dl.wait([m["id"] for m in PLAN["models"]])
        if PLAN.get("restart_comfy") and new_nodes:
            restart_comfy()
    except Exception as e:  # noqa: BLE001
        print(f"KIT_FETCH_FAILED {json.dumps({'workflows': ids, 'error': repr(e)[:300]})}", flush=True)
        return
    print(f"KIT_FETCHED {json.dumps({'workflows': ids, 'library': PLAN.get('library_ids') or []})}", flush=True)


def restart_comfy():
    """Restart ComfyUI in place so freshly installed custom nodes register (library installs)."""
    print("KIT_LIB restarting ComfyUI to load new custom nodes", flush=True)
    subprocess.run(["bash", "-lc", "pkill -f 'main.py --listen' || true"], check=False)
    for _ in range(30):
        if not up():
            break
        time.sleep(1)
    cfg = PLAN["comfyui"]
    cenv = {**os.environ, **{k: str(v) for k, v in (cfg.get("env") or {}).items()}}
    cenv.pop("CF_TUNNEL_TOKEN", None)
    cenv.pop("HF_TOKEN", None)
    subprocess.Popen(comfy_cmd(), cwd=COMFY, env=cenv, stdout=open(f"{LOG}/comfyui.log", "a"),
                     stderr=subprocess.STDOUT, start_new_session=True)
    for _ in range(300):
        if up():
            print("KIT_LIB ComfyUI restarted", flush=True)
            return
        time.sleep(1)
    raise RuntimeError("ComfyUI did not come back after restart")



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


def main():
    os.makedirs(LOG, exist_ok=True)
    if PLAN.get("fetch_only"):
        return fetch_only()
    if not PLAN.get("models"):  # wb-empty: nothing to download usually means a broken profile
        print("KIT_WARN no_models: this start profile has no models to download - check the profile", flush=True)
    cfg = PLAN["comfyui"]
    if up():
        print("ComfyUI already running on this VM; skipping setup.", flush=True)
        print("KIT_READY " + json.dumps({"reused": True}), flush=True)
        return
    ts("main_start")

    # Start model downloads immediately at T0 into RAM tmpfs to saturate the VM's ~7 Gbps network bandwidth.
    # Model downloads run concurrently with cloudflared installation, ComfyUI git clone, and pip dependencies.
    os.environ["HF_HUB_ENABLE_HF_TRANSFER"] = "1"
    os.environ["HF_HUB_DISABLE_XET"] = "1"
    os.environ["KIT_DL_CONNS"] = str(PLAN.get("download_connections", 8))
    if PLAN.get("xet_high_performance", True):
        os.environ["HF_XET_HIGH_PERFORMANCE"] = "1"  # only bites on the huggingface_hub fallback path
    os.environ["HF_HUB_DISABLE_PROGRESS_BARS"] = "1"  # tqdm bars flood the wake-page log

    store = setup_model_store(PLAN)
    if store:
        point_models_to(store)
    dl = Downloads(PLAN["models"], PLAN.get("download_concurrency", 8))
    dl.start()  # downloads run in parallel from T0 with everything below
    ts("downloads_started")

    ai_prep = ai_prepare()
    with Stage("bootstrap"):
        _wb_bootstrap(cfg)
        dl.sync_links()
        start_tunnel()
        sh("nvidia-smi --query-gpu=name,memory.total --format=csv,noheader", check=False)

    with Stage("deps"):
        frontend = cfg.get("frontend_package")
        sh(f"cd {COMFY} && grep -viE '^(torch|torchvision|torchaudio)([<>=~ ]|$)' requirements.txt > /tmp/req.txt && "
           + (f"(uv pip install --system -q -r /tmp/req.txt {repr(frontend) if frontend else ''} || "
              if PLAN.get("pip_installer", "uv") == "uv" else "(")
           + f"pip install -q -r /tmp/req.txt {repr(frontend) if frontend else ''})")
        install_custom_nodes()
        dl.sync_links()

    with Stage("install_workflows"):
        install_workflows()
        apply_settings()
        dl.sync_links()
    # ComfyUI is started BEFORE the last models land, so its ~30 s of startup (torch import,
    # custom nodes) overlaps the tail of the download instead of sitting on the critical path.
    # Safe because ComfyUI's filename cache is invalidated by folder mtime (folder_paths.py
    # cached_filename_list_), so the model list refreshes as soon as the files appear.
    for d in ("checkpoints", "clip", "clip_vision", "configs", "controlnet", "diffusers",
              "diffusion_models", "embeddings", "gligen", "hypernetworks", "loras", "model_patches",
              "scene_models", "style_models", "unet", "upscale_models", "vae", "vocoders"):
        os.makedirs(f"{COMFY}/models/{d}", exist_ok=True)
    ts("comfy_starting")
    # the boot runs in parallel with the remaining downloads -> tell the gate so the wake page
    # can show it as a concurrent step instead of a serial one
    print("KIT_PARALLEL booting start", flush=True)
    t_comfy = time.time()
    cenv = {**os.environ, **{k: str(v) for k, v in (cfg.get("env") or {}).items()}}
    cenv.pop("CF_TUNNEL_TOKEN", None)
    cenv.pop("HF_TOKEN", None)
    subprocess.Popen(comfy_cmd(), cwd=COMFY, env=cenv, stdout=open(f"{LOG}/comfyui.log", "w"),
                     stderr=subprocess.STDOUT, start_new_session=True)
    ts("download_wait")
    with Stage("download"):
        core = [m["id"] for m in PLAN["models"] if m.get("core")]
        dl.wait(core if PLAN.get("fast_start", True) else [m["id"] for m in PLAN["models"]])
        dl.sync_links()
    ts("comfy_check")
    with Stage("comfy_boot"):
        for _ in range(300):
            if up():
                break
            time.sleep(1)
        else:
            sh(f"tail -40 {LOG}/comfyui.log", check=False)
            raise RuntimeError("ComfyUI did not start")
        sh(f"grep -iE 'IMPORT FAILED|Traceback' {LOG}/comfyui.log | tail -10", check=False, quiet=True)
    # report the real boot time (it ran in parallel with the download, not after it)
    TIMES["comfy_boot"] = round(time.time() - t_comfy, 1)
    print(f"KIT_PARALLEL booting done {TIMES['comfy_boot']}", flush=True)
    ai_finish(ai_prep)
    if not any(t.name == "kit_watchdog" for t in threading.enumerate()):
        threading.Thread(target=watchdog, name="kit_watchdog", daemon=True).start()
    if PLAN.get("smoke"):
        with Stage("smoke"):
            smoke()
    ts("ready")
    spawn_watchdog()
    print("KIT_READY " + json.dumps({"total_s": round(time.time() - T0), "stages": TIMES,
                                     "background_downloads": len(PLAN["models"]) - dl.files_done,
                                     "keepalive": "gate-heartbeat"}), flush=True)
    # Return instead of looping forever: a cell that never finishes blocks the Jupyter kernel, and
    # every later `colab exec` (gate heartbeat, on-demand fetch) would queue behind it. The runtime
    # stays alive because the gate touches the kernel every ~90 s (Colab reclaims idle runtimes
    # after ~12 min), and the cost guard keeps running in its own process.


if __name__ == "__main__":
    if os.environ.get("KIT_WATCHDOG_ONLY") == "1":
        watchdog()
    else:
        try:
            main()
        except KitAbort:
            pass
