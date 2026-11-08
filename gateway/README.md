# 网关补丁

## 1.21.3 升级方式

已经应用引导 test6 和 Ollama 下载补丁、且测试正常的网关，无需因桌面正式版发布而重复部署或重启。桌面升级不会修改正在运行的 GPU。

仅修复启动下载时，将对应脚本上传 VPS 后执行：

```bash
python3 gate-bootstrap.py --container comfy-gate
python3 gate-ollama-download.py --container comfy-gate
```

两个脚本会自动定位实际挂载文件，先核对已知代码并备份后更新；支持 `--check` 只读检查，遇到未知自定义代码会拒绝覆盖。它们不重启容器、不启停 GPU，下次唤醒生效。引导补丁正式文件 `gate-bootstrap.py` 与已验证的 `gate-bootstrap-test6.py` 内容相同。

完整 AI 功能升级使用下方常规补丁流程。**如也需要引导修复，请在完整 `gate-ai.py` 之后应用 `gate-bootstrap.py`**：完整补丁默认恢复标准引导，执行顺序决定是否保留引导专用改进。已安装新版 Ollama 下载函数会被识别为无需更新。完整包还包含两份可选视觉补装脚本；无需看图时不运行它们。

引导补丁将 cloudflared 固定为官方 2026.10.7，提供官方资产 API 与发布地址回退，安装前校验完整大小/摘要；分别记录隧道程序安装、ComfyUI 克隆/固定提交检出/合并和 uv 安装。Ollama 固定为官方 v0.40.15，提供多个官方入口、完整校验和可恢复的安装替换。保持模型和 ComfyUI 固定版本。用户已确认本地和网关 Qwen 文字模式正常；官方下载网络仍可能间歇超时，不能保证所有启动都成功。

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
| `gate-ai.py` | 启动配置支持 `ai:` 块（llm / image）、`models/llm` 等更多模型目录、ComfyUI 内 `/wbllm` 路由转发到 VM 上的 Ollama；内置 Qwen3.8 27B、RealVisXL V5.0、Chroma1-HD 配置并写进 `.env` 的 `PROFILES`；软件「添加模型」用的 `GET/POST/DELETE /_gate/custom-profiles`（仅 owner / agent 令牌，存到 `STATE_DIR/custom_profiles/`，用 `parse_profile` 校验，最大 64 KB，使用中的配置删除返回 409）；修复大模型放不进内存时跳过克隆 ComfyUI 的问题 |

升级：直接用新版 `gate-ai.py` 再运行一次即可原地升级（旧补丁的区块会被整段替换），然后重建容器。补丁同时写入 `tests/test_wb_custom_profiles.py`。

1.21.3 的补丁增加 GGUF 导入复用：同一文件系统通过硬链接让 Ollama 复用已下载文件，保留标准 CLI 的注册与校验，不改变模型、精度、上下文或加载检查。无法建立链接时继续走原来的复制导入。仅更新桌面 exe 不会改变 VPS 的启动脚本；网关用户须在当前 GPU 会话结束后更新此补丁，再核对下一次唤醒的耗时，已有会话不会因此原地加速。

`gate-ai.py`、可选的 `gate-bootstrap.py` 和窄范围的 `gate-ollama-download.py` 由 `src/edits.py` + `src/build.py` 生成；下载函数来自 `src/ollama_installer.py`，窄补丁使用 `src/ollama_download_patch.py`。程序内嵌的 `kit/` 也用完整补丁升级：`python3 gateway/gate-ai.py kit`（之后删掉生成的 `.bak`）。

1.21.3 补丁另提供 `qwen38-27b-vision` 可选配置：保留原 Q8_0 文字配置，增加约 0.94 GB 的配套 GGUF 视觉组件。两份文件按固定仓库修订与 SHA-256 校验，仍用标准 Ollama CLI 导入；实际 `api/show` 必须返回视觉能力才进入就绪。真实 Colab 看图待验收。网关须在会话结束后更新补丁，下次唤醒选择新配置；不会更换现有运行中的模型。

已有 Qwen3.8 27B 会话也可补装：将 `enable-qwen-vision.py` 与 `enable-vision-on-vps.py` 放在 VPS 同一目录，运行 `python3 enable-vision-on-vps.py /opt/comfy-colab-kit`。它使用已有 Docker 服务 `comfy-gate`、网关设置和 Colab CLI，不重启网关、不停止或重新申请 GPU；通过本机认证请求与 VM 心跳保持安装活动。仅匹配固定 SHA-256/大小的内置 Q8_0 主模型，校验约 0.94 GB 视觉组件，保留原模型定义，标准 CLI 注册独立视觉模型，通过真实测试图片推理后原子更新 `wb_ai.json`。失败时保持原选择并尝试恢复原模型加载；不删除或释放 VM。请使用1.21.3 或更新的 exe，它按 VM 实际模型名称发送。实际部署结果仍需验收，脚本测试成功不代表生产已经安装。
