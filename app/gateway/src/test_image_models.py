"""Test image boot checks without importing CUDA or starting a GPU."""
import ast
import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import types
import unittest


class ImageModelChecks(unittest.TestCase):
    def test_boot_requires_files_and_comfy_index(self):
        source = (Path(__file__).resolve().parents[2] / 'kit/colab/setup.py').read_text(encoding='utf-8')
        func = next(n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name == 'ai_finish')
        code = compile(ast.Module(body=[func], type_ignores=[]), '<ai_finish>', 'exec')
        for case in ('valid', 'missing_file', 'empty_file', 'missing_index'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                dest = root / 'models/diffusion_models/chroma.safetensors'
                dest.parent.mkdir(parents=True)
                if case != 'missing_file':
                    dest.write_bytes(b'' if case == 'empty_file' else b'test')
                info = root / 'ai.json'
                info.write_text('{}')
                names = [] if case == 'missing_index' else ['chroma.safetensors']
                response = {'CheckpointLoaderSimple': {'input': {'required': {'ckpt_name': [names]}}}}
                env = dict(PLAN={'ai': {'type': 'image', 'models': ['m'], 'image': {'checkpoint': 'chroma.safetensors'}},
                                 'models': [{'id': 'm', 'dest': 'diffusion_models/chroma.safetensors'}]},
                           COMFY=temp, AI_INFO=str(info), PORT=8188, os=os, json=json,
                           Stage=lambda _: contextlib.nullcontext(),
                           urllib=types.SimpleNamespace(request=types.SimpleNamespace(urlopen=lambda *a, **kw: io.StringIO(json.dumps(response)))))
                exec(code, env)
                if case == 'valid':
                    env['ai_finish'](None)
                    self.assertTrue(json.loads(info.read_text())['ready'])
                else:
                    with self.assertRaises(RuntimeError):
                        env['ai_finish'](None)
                    self.assertNotIn('ready', json.loads(info.read_text()))


if __name__ == '__main__':
    unittest.main()
