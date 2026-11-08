"""Test the running-VM vision installer without downloading weights or using a GPU."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("enable_vision", ROOT / "gateway/enable-qwen-vision.py")
v = importlib.util.module_from_spec(spec)
spec.loader.exec_module(v)


class EnableVisionTests(unittest.TestCase):
    def test_pinned_weights_match_startup_profile(self):
        profile = (ROOT / "kit/profiles/qwen38-27b-vision/profile.yaml").read_text(encoding="utf-8")
        self.assertIn(v.MAIN_SHA, profile)
        self.assertIn(v.PROJECTOR_SHA, profile)
        self.assertIn(v.REV, profile)

    def test_real_image_request_before_switch_and_old_info_preserved_on_failure(self):
        for case in ("ok", "create_error", "no_vision", "load_error", "image_error", "wrong_model", "already"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()), patch.object(v, "MAIN_BYTES", 4):
                root = Path(temp)
                models = root / "models"
                blob = models / "blobs" / ("sha256-" + v.MAIN_SHA)
                blob.parent.mkdir(parents=True)
                blob.write_bytes(b"GGUF" if case != "wrong_model" else b"wrong")
                info_path = root / "info.json"
                original = json.dumps({"type": "llm", "ready": True, "llm": {"name": "qwen3.8-27b", "ctx": 32768}})
                info_path.write_text(original)
                calls, creates, downloads = [], [], []

                def api(path, body, **kwargs):
                    calls.append((path, body))
                    if path == "show":
                        enabled = case == "already" or body["model"] == v.VISION_NAME and case != "no_vision"
                        return {"capabilities": ["completion"] + (["vision"] if enabled else [])}
                    if path == "generate" and body["model"] == v.VISION_NAME and case == "load_error":
                        raise RuntimeError("GPU load failed")
                    if path == "chat":
                        # Real image data, bounded output and the configured context reach Ollama.
                        self.assertEqual(body["options"]["num_ctx"], 32768)
                        self.assertTrue(body["messages"][0]["images"][0].startswith("iVBOR"))
                        self.assertEqual(info_path.read_text(), original)
                        return {"error": "image failed"} if case == "image_error" else {"message": {"content": "red"}}
                    return {}

                def create(*args):
                    creates.append(args)
                    if case == "create_error":
                        raise RuntimeError("create failed")

                if case in ("ok", "already"):
                    v.enable_vision(info_path, models, api, downloads.append, create)
                    if case == "ok":
                        self.assertEqual(json.loads(info_path.read_text())["llm"]["name"], v.VISION_NAME)
                        self.assertTrue(any(path == "chat" for path, _ in calls))
                        self.assertEqual(len(downloads), 1)
                    else:
                        self.assertEqual(creates, [])
                        self.assertEqual(downloads, [])
                else:
                    with self.assertRaises(RuntimeError):
                        v.enable_vision(info_path, models, api, downloads.append, create)
                    self.assertEqual(info_path.read_text(), original)
                    if case in ("load_error", "image_error"):
                        self.assertEqual(calls[-1][1]["model"], "qwen3.8-27b")
                # Never an API or CLI instruction that stops/releases a VM or deletes the old model.
                self.assertTrue(all(path in ("show", "generate", "chat") for path, _ in calls))

    def test_download_checks_size_and_hash_before_commit(self):
        raw = b"GGUF-projector-test"
        import hashlib
        for valid in (True, False):
            with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
                dest = Path(temp) / "mmproj.gguf"
                with patch.object(v, "PROJECTOR_BYTES", len(raw)), patch.object(v, "PROJECTOR_SHA", hashlib.sha256(raw).hexdigest()), patch.object(v.urllib.request, "urlopen", return_value=io.BytesIO(raw if valid else b"bad")):
                    if valid:
                        v.download_projector(dest)
                        self.assertEqual(dest.read_bytes(), raw)
                    else:
                        with self.assertRaises(RuntimeError):
                            v.download_projector(dest)
                        self.assertFalse(dest.exists())
                self.assertEqual(list(Path(temp).glob("*.part")), [])

    def test_vps_wrapper_uses_existing_cli_session_without_restart(self):
        helper = ROOT / "gateway/enable-vision-on-vps.py"
        spec = importlib.util.spec_from_file_location("vps_helper", helper)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        code = module.container_code("print('fixture')")
        compile(code, "<vps installer>", "exec")
        self.assertIn("cli.exec_stream(s.colab_session_name", code)
        self.assertNotIn("cli.stop", code)
        self.assertNotIn("cli.new", code)


if __name__ == "__main__":
    unittest.main()
