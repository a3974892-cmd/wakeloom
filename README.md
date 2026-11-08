# Wakeloom · 唤织

**按需唤醒云端 GPU，生成视频、图片与对话的 Windows 桌面工作台。**
A Windows desktop workbench that wakes a Colab GPU on demand and turns it into a video / image / chat studio — then puts it back to sleep to save compute units.

[English](#english) · [下载](#下载) · [更新日志](CHANGELOG.md)

项目维护者：[a3974892-cmd](https://github.com/a3974892-cmd)

维护资料：[项目交接](docs/PROJECT_HANDOFF.md) · [待办与验收](docs/ROADMAP.md) · [维护入口](AGENTS.md)

> **第一次使用？看 [新手指南](docs/GETTING_STARTED.md)。** 软件第一次打开时也会自动带你走一遍：用 Google 账号登录，点三下就能开机。

---

## 它能做什么

- **一键开关 GPU**：唤醒 / 停止 / 延长 Colab GPU，可选 GPU 型号（T4 / L4 / G4 / A100 / H100），实时启动日志、计时、CU 余额与 GPU 负载面板；空闲自动停机省 CU。
- **按需加载**：用「启动配置」决定这次开机装哪些模型和插件，只下载需要的东西。
- **生成视频**：导入 ComfyUI「导出 (API)」工作流，自动识别提示词、图片、种子、尺寸等参数（中文名 + 推荐值）；内置 MiniMax H3 提示词骨架与 AI 写提示词。
- **任务队列**：串行提交、进度、取消、重试、重启后续跑；低清试生成满意后一键出高清；结果自动下载到本地。
- **AI 页**：
  - 对话：Qwen3.8 27B（GGUF，Ollama 运行在你的 GPU 上），多轮对话、深度思考开关、一键把回复用作生成提示词。
  - 对话附件与技能：添加文档、代码、图片、文件夹或 ZIP；从本地或公开 GitHub 安装 SKILL.md 技能并按模型启用。图片需当前部署模型具备视觉能力；技能中的脚本与外部工具不会自动执行。对话、附件与技能保存在本机。
  - 生图：RealVisXL V5.0（SDXL 写实）、Chroma1-HD（FLUX 架构）。
  - **自定义添加模型**：粘贴 Hugging Face 链接，自动识别类型（GGUF 对话 / SDXL / Chroma / 任意 ComfyUI 模型）、大小与许可证，推荐 GPU 和参数，一键生成启动配置。
- **两种连接方式**：
  - **网关模式**：连接你自己部署的 [comfy-colab-kit](https://github.com/FireTable/comfy-colab-kit) 网关（VPS），可多人共用。
  - **本地模式（实验）**：不需要 VPS，软件直接用你的 Google 账号开关 Colab。
- **自动更新**：从 GitHub Releases 下载并校验 SHA-256 后替换。

## 下载

Linux 测试版已加入源码，提供 amd64 / arm64 单文件与便携包，浏览器打开同一工作台；使用方法、数据目录及验证范围见 [Linux 说明](docs/LINUX.md)。正式 Linux Release 尚未发布。

到 [Releases](../../releases/latest) 下载最新的 `Wakeloom-x.y.z.exe`，双击运行即可（单文件，无需安装；需要 Windows 10/11 自带的 WebView2）。

> 安装包没有代码签名，首次运行 Windows 可能提示「Windows 已保护你的电脑」（SmartScreen）：点 **更多信息**，再点 **仍要运行**。
>
> ![SmartScreen：更多信息 → 仍要运行](docs/img/smartscreen.png)

**校验文件（可选）：** Release 里的 `latest.json` 写着 exe 的 `sha256`。在下载目录打开 PowerShell 运行 `Get-FileHash .\Wakeloom-x.y.z.exe -Algorithm SHA256`（或命令提示符 `certutil -hashfile Wakeloom-x.y.z.exe SHA256`），两边的值应该完全一样。

## 你需要准备什么

| | 网关模式 | 本地模式（实验） |
|---|---|---|
| Colab 算力 | Colab Pro / Pay As You Go 的 CU | 同左 |
| 服务器 | 一台 VPS 部署 comfy-colab-kit 网关 | 不需要 |
| 配置 | 网关地址 + 令牌（可选 Cloudflare Access 服务令牌） | 软件内登录 Google |

网关模式的增强功能（选择 GPU、插件/模型库、AI 页）需要给网关打补丁，见 [gateway/](app/gateway/README.md)。

### ⚠️ 本地模式说明

本地模式通过 Colab 的非公开接口工作，登录使用的是 Google Cloud SDK 的公开 OAuth 客户端。这不是 Google 官方支持的用法，**接口可能随时变化，也存在账号被限制的风险**，请自行评估后使用。长期稳定使用推荐网关模式。

## 模型与内容

AI 页的模型来自第三方，使用时请遵守各自的许可证：

| 模型 | 来源 | 许可 |
|---|---|---|
| Huihui-Qwen3.8-27B-abliterated (Q8_0) | [huihui-ai](https://huggingface.co/huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF) | Apache-2.0（去除了拒答的版本） |
| RealVisXL V5.0 | [SG161222](https://huggingface.co/SG161222/RealVisXL_V5.0) | CreativeML OpenRAIL++-M |
| Chroma1-HD | [lodestones](https://huggingface.co/lodestones/Chroma1-HD) | Apache-2.0 |
| MiniMax H3 | [Comfy-Org](https://huggingface.co/Comfy-Org/MiniMax-H3) | MiniMax H3 社区许可 |

> MiniMax H3 社区许可的适用地区不包括美国、英国、欧盟和韩国，请自行确认你所在地区是否适用。Wakeloom 不附带 MiniMax 官方的 H3 提示词指南；如需使用，请自行从 MiniMax 官方获取并在「设置 → 大模型 API」中导入。

部分模型没有内容过滤。请遵守法律、模型许可证和 [Colab 使用条款](https://research.google.com/colaboratory/faq.html)，不要生成违法内容、未经同意的真实人物内容或任何涉及未成年人的不当内容。

## 从源码构建

需要 Go 1.24+（可在 Linux / macOS 上交叉编译）：

```bash
cd app
go test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H windowsgui" -o Wakeloom.exe .
```

图标 / 版本资源改动后重新生成：`go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --arch amd64 --out rsrc`

发版：修改 `app/update.go` 的 `AppVersion`、在 `CHANGELOG.md` 顶部写 `## vX.Y.Z`，推送 main 后自动发布（也可选择推送标签 `vX.Y.Z`，或在 Actions 页面手动运行 **release** 工作流，三种方式只选一种），GitHub Actions 会自动测试、构建并发布 exe 和 `latest.json`。

## 目录

```
app/            程序源码、测试和依赖
  ui/           内嵌界面和图标
  kit/          Colab 启动套件与模型配置
  gateway/      网关补丁及其源码
docs/           使用指南和维护记录
scripts/        构建与检查工具
.github/        自动测试和发布流程
```

## 致谢

- [comfy-colab-kit](https://github.com/FireTable/comfy-colab-kit)（FireTable，MIT）—— 网关与 Colab 启动套件
- [ComfyUI](https://github.com/comfyanonymous/ComfyUI)、[Ollama](https://github.com/ollama/ollama)、[go-webview2](https://github.com/jchv/go-webview2)

本项目与 Google、Comfy Org 无关联。

---

## English

**Wakeloom** is a single-file Windows app that wakes a Google Colab GPU when you need it, runs ComfyUI workflows (video, image) and local LLM chat on it, downloads the results, and shuts the GPU down when idle.

- Gateway mode talks to your own [comfy-colab-kit](https://github.com/FireTable/comfy-colab-kit) gateway (apply the patches in `app/gateway/` for GPU selection, the add-on library and the AI page).
- Local mode (experimental) drives Colab directly with your Google account. It relies on non-public Colab endpoints and the public Google Cloud SDK OAuth client — use at your own risk.
- AI page: Qwen3.8 27B (GGUF via Ollama), RealVisXL V5.0, Chroma1-HD; add more with a profile.
- Releases are built by GitHub Actions; the app auto-updates from them (SHA-256 verified).

Build: `cd app` first, then `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H windowsgui" -o Wakeloom.exe .`

MIT License. `app/kit/` is derived from comfy-colab-kit (MIT, © FireTable). Not affiliated with Google or Comfy Org.
