# 更新日志

## v1.19.0
- 新增「自定义添加模型」：在 AI 页点「＋ 添加模型」或在「设置 → 自定义模型」里粘贴 Hugging Face 链接，自动识别类型（GGUF 对话大模型、SDXL 检查点、Chroma 生图、任意 ComfyUI 模型）、文件大小、许可证，推荐 GPU 和参数，保存后直接出现在唤醒选项和 AI 页。
- GGUF 仓库列出全部量化版本，默认 Q4_K_M，显存够时选 Q8_0；忽略 mmproj 和分片文件。
- 「任意 ComfyUI 模型」可添加多个文件、选择存放目录，并关联「生成」页的模板（排队时自动用它唤醒）。
- 本地模式可在设置里填 Hugging Face 令牌，用于下载需申请访问（gated）的模型；令牌只存在本机，不写进日志或启动配置。
- 启动配置支持更多 ComfyUI 模型目录（style_models、ipadapter、ultralytics 等）。
- 网关模式需要更新网关补丁：在 VPS 上重新运行 `python3 gate-ai.py /opt/comfy-colab-kit`，再重建容器（`docker compose -f deploy/docker-compose.yml up -d --force-recreate`）；未更新时软件会提示「网关需要更新 gate-ai.py」。

## v1.18.1
- 大模型唤醒时「注册模型到 Ollama」一步每 10 秒输出一次进度（已用时间、已写入大小、Ollama 状态），不再看起来像卡住。
- 发版工作流升级到 actions/checkout@v7、actions/setup-go@v7（Node 24）。

## v1.18.0
- 发布到 GitHub；自动更新改为从 GitHub Releases 下载。

## v1.17.1
- 修复：Qwen 对话配置唤醒时 Ollama 安装失败（不再依赖官方安装脚本和系统包管理器）。

## v1.17.0
- 改名 Wakeloom（唤织）；设置自动从旧目录迁移。
- AI 页新增 Chroma1-HD 生图。
- 修复：大模型唤醒时「requirements.txt 不存在」导致安装依赖失败。

## v1.16.0
- 新增「AI」页：Qwen3.8 27B 对话（Ollama）、RealVisXL V5.0 生图。

## v1.15.0
- 唤醒时可选择 GPU。
