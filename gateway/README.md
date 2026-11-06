# 网关补丁

给 [comfy-colab-kit](https://github.com/FireTable/comfy-colab-kit) 网关打的一键补丁。每个脚本都可以重复运行：已经打过的修改会跳过，改动前会留 `.bak` 备份，代码对不上时什么都不改并退出。

在 VPS 上以 root 运行（假设网关在 `/opt/comfy-colab-kit`）：

```bash
python3 gate-gpu-select.py /opt/comfy-colab-kit   # 唤醒时可选 GPU
python3 gate-library.py    /opt/comfy-colab-kit   # 插件 / 模型库（/_gate/library）
python3 gate-ai.py         /opt/comfy-colab-kit   # AI 页：Ollama 对话 + 生图配置（需先打 gate-gpu-select）
cd /opt/comfy-colab-kit && docker compose -f deploy/docker-compose.yml up -d --force-recreate
```

| 脚本 | 作用 |
|---|---|
| `gate-gpu-select.py` | `/_gate/wake` 接受 `accelerator`，`/_gate/status` 返回本次与默认 GPU |
| `gate-library.py` | 保存插件/模型清单，按需装到运行中的 GPU，或每次开机自动装 |
| `gate-ai.py` | 启动配置支持 `ai:` 块（llm / image）、`models/llm` 目录、ComfyUI 内 `/wbllm` 路由转发到 VM 上的 Ollama；内置 Qwen3.8 27B、RealVisXL V5.0、Chroma1-HD 配置并写进 `.env` 的 `PROFILES`；修复大模型放不进内存时跳过克隆 ComfyUI 的问题 |

`gate-ai.py` 由 `src/edits.py` + `src/build.py` 生成：改 `edits.py` 后运行 `python3 src/build.py`。程序内嵌的 `kit/` 也用它升级：`python3 gateway/gate-ai.py kit`（之后删掉生成的 `.bak`）。
