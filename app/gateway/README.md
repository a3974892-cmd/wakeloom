# 网关补丁

## 1.21.5-test7：跨文件系统模型复用（未正式发布）

补齐 test6 未启用 RAM 存储时仍因空间不足无法导入的路径。对已完成下载的模型校验完整 SHA-256 和文件身份后，同文件系统使用硬链接，跨文件系统使用绝对符号链接，Ollama 继续通过原 CLI 注册。原模型不能在 VM 使用期间删除；补丁不会移动源模型或覆盖已有 blob。复用失败仍检查完整复制预算。

约 48.7 GB 模型在复用成功时，额外转换预算约 55.5 GB，而非复制加转换的 104.2 GB；不修改模型量化、窗口或绕过兼容转换。日志记录设备和链接类型。本地使用 test7 exe 即可，网关仍需主动应用补丁才生效。本机模拟/回归已检查，Windows 无权限的原生符号链接测试跳过，真实 Colab 70B 导入仍待验证。本次未部署 VPS。

## 1.21.5-test6：大模型导入空间修复（未正式发布）

针对 DeepSeek 70B 等大 GGUF 在 `compat-migrate-*.gguf` 阶段报 `no space left on device`：启动 Ollama 前核对复制及兼容转换预算。磁盘不足、已有 RAM 模型存储且保留 24 GB 运行内存后仍足够时，扩容原 tmpfs 并让 Ollama 使用同一文件系统，避免额外源文件副本；导入前再次检查真实剩余容量。内存不足或扩容失败不强行切换，空间不够会在导入前说明原因。未改变模型、量化或窗口。

本地模式只需 test6 exe，原自定义配置继续使用。网关需要应用新套件才有此修复，但本次未部署任何 VPS；正常网关不必因本地问题立即更新。更新流程及自定义缓存保留要求同下。容量模拟、生成器幂等及软件回归通过，真实 Colab 70B 导入尚待实测。

## 1.21.5-test5：桌面自动执行实验（未正式发布）

DeepSeek-R1-70B-Q5 与 Qwen3.8 的自动执行器运行在 Windows 桌面程序，通过现有聊天接口调用模型。本地模式和普通 Ollama 网关模式只需更新测试 exe；无需因此重新部署网关或 GPU。网页端未增加电脑控制功能。

本包另将独立 1M 适配层的 `format: "json"` 转为 llama.cpp 的 JSON 输出请求，用于自动任务；普通聊天路径不变。仅使用已有 Ollama 配置时不要为这个小改动覆盖正常网关。1M 仍遵循下方实验配置要求，升级服务器前保留自定义缓存补丁并等待当前会话结束。本次交付没有自动更新 VPS。

## 1.21.5-test4：可选 1M 实验配置（未正式发布）

新增独立 `qwen38-27b-1m`，原文字/看图配置仍为 32K。1M 配置用固定 llama.cpp b12211 CUDA 12.8 官方安装包（大小和 SHA-256 校验）、YaRN ×4、Q8 KV 缓存与单个 1,000,000-token 槽位。仅支持文字、文档和程序静态报告，至少 80 GB 显存，默认 G4。实际显存需求、Colab 启动与满窗口检索质量尚未实机验收；测试通过仅指模拟服务。Huihui 修改版的 1M 效果不能由官方原模型说明直接推定。

本地模式使用 test4 exe 自带配置即可。网关模式需要先备份网关目录和 `.env`，等当前 GPU 会话结束，再按下方完整 AI 升级流程应用 `gate-ai.py`、重新应用 `gate-bootstrap.py`，最后重建网关容器；仅更新 exe 不会更新 VPS。已有自定义下载缓存补丁需要核对并保留，不要对仍在正常使用的网关盲目覆盖。本次交付未操作任何 VPS。

启动时核对实际分配的窗口，并进行短回复检查后才显示就绪；该检查不是满 1M 效果验收。发送前用服务端模板和 tokenizer 核对输入，预留 16,384 tokens 回复和 4,096 tokens 余量，输入预算 979,520 tokens。历史/附件可能选段，界面披露带入情况。不能把 `LONG_CONTEXT=True` 加到原 Ollama 配置来替代这条运行路径。

恢复普通模式时，在用户结束实验会话后重新选择原文字或看图配置唤醒即可。聊天按启动配置分别保存，原有记录不删除，也不会自动迁移到 1M 配置。

依据：[Qwen 官方长上下文说明](https://huggingface.co/Qwen/Qwen3.8-27B#best-practices)、[固定 llama.cpp 版本](https://github.com/ggml-org/llama.cpp/releases/tag/b12211)。静态 YaRN 扩展可能影响短输入效果，1M 不等于永久记忆。

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

`gate-ai.py`、可选的 `gate-bootstrap.py` 和窄范围的 `gate-ollama-download.py` 由 `src/edits.py` + `src/build.py` 生成；下载函数来自 `src/ollama_installer.py`，窄补丁使用 `src/ollama_download_patch.py`。程序内嵌的 `kit/` 也用完整补丁升级：从仓库根目录执行 `cd app` 后，运行 `python3 gateway/gate-ai.py kit`（之后删掉生成的 `.bak`）。

1.21.3 补丁另提供 `qwen38-27b-vision` 可选配置：保留原 Q8_0 文字配置，增加约 0.94 GB 的配套 GGUF 视觉组件。两份文件按固定仓库修订与 SHA-256 校验，仍用标准 Ollama CLI 导入；实际 `api/show` 必须返回视觉能力才进入就绪。真实 Colab 看图待验收。网关须在会话结束后更新补丁，下次唤醒选择新配置；不会更换现有运行中的模型。

已有 Qwen3.8 27B 会话也可补装：将 `enable-qwen-vision.py` 与 `enable-vision-on-vps.py` 放在 VPS 同一目录，运行 `python3 enable-vision-on-vps.py /opt/comfy-colab-kit`。它使用已有 Docker 服务 `comfy-gate`、网关设置和 Colab CLI，不重启网关、不停止或重新申请 GPU；通过本机认证请求与 VM 心跳保持安装活动。仅匹配固定 SHA-256/大小的内置 Q8_0 主模型，校验约 0.94 GB 视觉组件，保留原模型定义，标准 CLI 注册独立视觉模型，通过真实测试图片推理后原子更新 `wb_ai.json`。失败时保持原选择并尝试恢复原模型加载；不删除或释放 VM。请使用1.21.3 或更新的 exe，它按 VM 实际模型名称发送。实际部署结果仍需验收，脚本测试成功不代表生产已经安装。
