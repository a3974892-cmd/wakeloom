"""Cache failures and upgrade compatibility, without a VPS or GPU."""
import ast
import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import threading
import types
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    obj = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(obj)
    return obj


CACHE = module("download_cache", ROOT / "gateway/prepare-download-cache.py")
BOOT = module("boot_patch_for_cache", ROOT / "gateway/gate-bootstrap.py")


class Response(io.BytesIO):
    def __init__(self, payload, content_range, status=206, fail=False):
        super().__init__(payload)
        self.status = status
        self.headers = {"Content-Range": content_range}
        self.fail = fail
        self.reads = 0

    def read(self, size=-1):
        self.reads += 1
        if self.fail and self.reads == 2:
            raise TimeoutError("test only")
        return super().read(min(2, size) if self.fail else size)


class CacheTests(unittest.TestCase):
    def env(self, root, data=b"abcdefghijkl"):
        entry = {"file": "package.deb", "size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
        (root / CACHE.PRIVATE_NAME).write_text(json.dumps({"base_url": "https://gateway.invalid/_gate/static/wb-installers/",
            "headers": {"CF-Access-Client-Secret": "secret-test-value"}, "assets": {"cloudflared": entry}}), encoding="utf-8")
        env = {"__file__": str(root / "setup.py"), "_wb_bootstrap_download": lambda *a: None,
               "_ollama_install_archive": lambda *a: None}
        exec(CACHE.RUNTIME.replace("32 * 1024 * 1024", "4"), env)
        return env, entry

    def test_range_failure_resumes_at_boundary_and_verifies_complete_file(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); data = b"abcdefghijkl"; env, entry = self.env(root, data)
            calls, notes = [], []
            def fetch(req, **kwargs):
                byte_range = req.get_header("Range"); calls.append(byte_range)
                first, last = map(int, byte_range[6:].split("-"))
                return Response(data[first:last+1], f"bytes {first}-{last}/{len(data)}", fail=len(calls)==2)
            with patch("urllib.request.OpenerDirector.open", side_effect=fetch):
                self.assertTrue(env["_wb_cached_package"]("cloudflared", str(root/"out"), entry["size"], entry["sha256"], notes.append))
            self.assertEqual((root/"out").read_bytes(), data)
            self.assertEqual(calls, ["bytes=0-3", "bytes=4-7", "bytes=4-7", "bytes=8-11"])
            self.assertIn("gateway cache size and SHA-256 verified", notes)

    def test_wrong_hash_login_page_and_wrong_range_never_succeed(self):
        for kind in ("hash", "login", "range"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as temp:
                root=Path(temp); env, entry=self.env(root); notes=[]
                def fetch(req, **kwargs):
                    a,b=map(int, req.get_header("Range")[6:].split("-"))
                    return Response(b"xxxx", f"bytes {a}-{b}/12" if kind != "range" else "bytes 0-3/999",
                                    status=200 if kind == "login" else 206)
                with patch("urllib.request.OpenerDirector.open", side_effect=fetch):
                    self.assertFalse(env["_wb_cached_package"]("cloudflared", str(root/"out"), entry["size"], entry["sha256"], notes.append))
                self.assertFalse((root/"out").exists())
                self.assertNotIn("secret-test-value", " ".join(notes))

    def test_missing_config_invalid_origin_cancel_and_redirect(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp); env,entry=self.env(root); cfg=root/CACHE.PRIVATE_NAME
            original=json.loads(cfg.read_text()); bad=dict(original);bad['base_url']='http://outside.invalid/'
            cfg.write_text(json.dumps(bad));notes=[]
            with patch("urllib.request.OpenerDirector.open") as request:
                self.assertFalse(env["_wb_cached_package"]("cloudflared",str(root/'out'),12,entry['sha256'],notes.append))
                self.assertFalse(request.called)
            cfg.write_text(json.dumps(original)); cancel=threading.Event();cancel.set()
            with self.assertRaisesRegex(RuntimeError,"canceled"):
                env["_wb_cached_package"]("cloudflared",str(root/'out'),12,entry['sha256'],notes.append,cancel)
            cfg.unlink()
            self.assertFalse(env["_wb_cached_package"]("cloudflared",str(root/'out'),12,entry['sha256'],notes.append))
            self.assertIsNone(CACHE.NoRedirect().redirect_request(None,None,302,'',{},'https://elsewhere.invalid'))

    def test_patch_is_idempotent_and_preserves_original_code(self):
        original=BOOT.patch_source((ROOT/'kit/colab/setup.py').read_text(encoding='utf-8'))
        updated=CACHE.patch_source(original)
        self.assertEqual(CACHE.patch_source(updated),updated)
        a=updated.index(CACHE.START);b=updated.index(CACHE.END)+len(CACHE.END)
        self.assertEqual(ast.dump(ast.parse(updated[:a]+updated[b:])),ast.dump(ast.parse(original)))
        with self.assertRaisesRegex(RuntimeError,'Unrecognized'):
            CACHE.patch_source(original.replace('timeout=15','timeout=999'))
        with self.assertRaisesRegex(RuntimeError,'Different cache'):
            CACHE.patch_source(updated.replace('gateway cache unavailable','custom cache error'))

    def test_mount_selection_and_private_file_permissions(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);(root/'colab').mkdir()
            mounts=[{'Type':'bind','Source':str(root),'Destination':'/app/packages'}]
            self.assertEqual(CACHE.mounted_dir(mounts,'/app/packages/colab'),(root/'colab').resolve())
            with self.assertRaises(RuntimeError):CACHE.mounted_dir(mounts,'/app/apps/gate/web')
            # Ownership requires a real Linux deployment check; Windows has no os.chown.
            with patch.object(CACHE.os,'chown',create=True):
                CACHE.atomic_write(root/'private.json',b'private fixture',0o600,10001,10001)
            self.assertEqual((root/'private.json').read_bytes(),b'private fixture')

    def test_preparation_reuses_verified_cache_and_keeps_existing_file_on_failure(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()):
            root=Path(temp);data=b'!<arch>\nfixture';target=root/'asset.deb';target.write_bytes(data)
            asset={'file':target.name,'size':len(data),'sha256':hashlib.sha256(data).hexdigest(),
                   'urls':['https://api.invalid/asset']}
            with patch('urllib.request.urlopen') as fetch:
                self.assertEqual(CACHE.cache_asset(root,asset),target)
                self.assertFalse(fetch.called)
            target.write_bytes(b'old invalid cache')
            response=Response(b'bad', 'ignored',status=200)
            with patch('urllib.request.urlopen',return_value=response):
                with self.assertRaisesRegex(RuntimeError,'Official download routes failed'):
                    CACHE.cache_asset(root,asset)
            self.assertEqual(target.read_bytes(),b'old invalid cache')
            self.assertEqual(list(root.glob('.wb-download-*')),[])

    def test_preflight_and_activation_keep_credentials_private_and_do_not_restart(self):
        with tempfile.TemporaryDirectory() as temp, contextlib.redirect_stdout(io.StringIO()) as log:
            root=Path(temp);colab=root/'colab';web=root/'web';colab.mkdir();web.mkdir()
            source=BOOT.patch_source((ROOT/'kit/colab/setup.py').read_text(encoding='utf-8'))
            setup=colab/'setup.py';setup.write_text(source,encoding='utf-8');raw=setup.read_bytes()
            meta={'state':'sleeping','host':'gateway.invalid','uid':10001,'gid':10001,
                  'headers':{'CF-Access-Client-Secret':'private-fixture'}}
            fake_os=types.SimpleNamespace(**vars(CACHE.os))
            fake_os.name='posix';fake_os.geteuid=lambda:0;fake_os.chown=lambda *a:None
            mounts=[{'Type':'bind','Source':str(colab),'Destination':'/app/packages/colab'},
                    {'Type':'bind','Source':str(web),'Destination':'/app/apps/gate/web'}]
            def inspected(command,**kwargs):
                self.assertEqual(command[:2],['docker','inspect'])
                return types.SimpleNamespace(stdout=json.dumps(mounts))
            def health(*args,**kwargs):
                return io.BytesIO(b'{"ok":true}')
            with patch.object(CACHE,'os',fake_os), patch.object(CACHE,'container_info',return_value=meta), \
                 patch.object(CACHE.subprocess,'run',side_effect=inspected), \
                 patch('urllib.request.OpenerDirector.open',side_effect=health), \
                 patch.object(CACHE,'cache_asset') as download, patch.object(CACHE,'check_access') as access:
                with patch('sys.argv',['helper','--check']):CACHE.main()
                self.assertFalse(download.called);self.assertFalse(access.called)
                self.assertEqual(setup.read_bytes(),raw)
                self.assertFalse((colab/CACHE.PRIVATE_NAME).exists())
                with patch('sys.argv',['helper']):CACHE.main()
                self.assertEqual(download.call_count,2);access.assert_called_once()
                self.assertIn(CACHE.START,setup.read_text(encoding='utf-8'))
                backups=list(colab.glob('setup.py.wb-cache-*.bak'))
                self.assertEqual(len(backups),1);self.assertEqual(backups[0].read_bytes(),raw)
                cfg=json.loads((colab/CACHE.PRIVATE_NAME).read_text())
                self.assertEqual(cfg['headers']['CF-Access-Client-Secret'],'private-fixture')
                self.assertFalse((web/CACHE.PRIVATE_NAME).exists())
                self.assertNotIn('private-fixture',setup.read_text(encoding='utf-8'))
                meta['state']='ready'
                with patch('sys.argv',['helper']),self.assertRaisesRegex(RuntimeError,'sleeping'):CACHE.main()
            self.assertNotIn('private-fixture',log.getvalue())


if __name__ == '__main__':
    unittest.main()
