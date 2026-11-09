# CLAUDE.md — Wakeloom（唤织）维护说明

Windows 桌面程序：按需唤醒 Colab GPU 跑 ComfyUI 生成视频/图片，AI 页用 Ollama 跑 GGUF 大模型对话 + SDXL/Chroma 生图，空闲自动停机。
Windows 为单文件 exe（Go + WebView2）；Linux 为无 cgo 的单文件程序，在默认浏览器打开界面。界面、启动套件全部 `go:embed`。

Linux：`python3 scripts/build-linux.py` 生成 amd64/arm64 静态程序、便携 tar.gz 与分架构清单；本地测试可加 `--label=-test1 --out <目录>`（不生成正式更新清单）。`scripts/smoke-linux.py <本机架构程序>` 使用临时数据目录验证 HTTP、重复启动与退出后重开。Linux 使用 XDG_CONFIG_HOME 或 ~/.config、内核文件锁（含 headless）、独立 latest-linux-<arch>.json，不能回退到 Windows 更新清单。Linux 平台测试需要在 Linux 执行，交叉编译通过不等于运行验收。

**合规：** 不要把 MiniMax 官方 H3 提示词指南（或其中的句子）放进仓库或 exe——其社区许可限制分发地区。提示词骨架、片段、内置说明都要用自己的措辞，只保留模型要求的字段名（如 `integrated_multimodal_description`、`<Picture 1>`、`[Shot 1]`）。

**当前维护基线：v1.21.5 正式版（源自 test9-fix3，含桌面图标迁移）。** 发布标签、Windows 附件、哈希及 latest 自动更新入口已验证；Linux 不发布。 新增申请独立 HTTP/1.1 与断线同 notebook 查询；用户确认本地账户官网也无法获得 G4，不能宣称该兼容改动修复账户资源限制。 在 test9-fix1 上修复本地分配请求 60 秒提前超时，以及 1M slot 对齐误判；真实 GPU 仍待验收。 用户已取消 Kaggle；恢复 test9，仅保留 test10 自动执行弹窗背景防误关闭。撤回源码已私有备份，不应按历史聊天重新添加 Kaggle。完整验证记录见交接文档。

## 目录结构

源码已集中到 `app/`。下表代码路径相对 `app/`；`docs/`、`scripts/`、`.github/` 和 `CHANGELOG.md` 相对仓库根目录。构建及套件维护命令先 `cd app`，发布脚本从仓库根目录运行。

| 路径 | 内容 |
|---|---|
| `*.go`（module `ckwb`，package main） | 桌面程序：本地 HTTP 服务 + WebView2 窗口 |
| `main.go` | 入口：单实例、数据目录、配置、本地网关、任务 worker、本地 HTTP 服务，再开窗口 |
| `server.go` | 本地 HTTP API，`//go:embed ui` |
| `diagnostics.go` / `ui/diagnostics.js` | 本机诊断日志与预览/复制/导出；写盘前脱敏，不记录请求正文或配置。约 6 MB 轮换，最近 3000 条展示；网关日志仅在软件连接期间收集。测试版本用链接参数 `-X main.BuildLabel=...` 标识报告 |
| `ui/refinement.css` / `ui/brand.svg` | 界面排版、图标导航、浅深色与窄窗口适配；原创 W 标志。exe 图标由 `winres/icon.png` 生成，修改后须更新 Windows 资源 |
| `gate.go` / `proxy*.go` | 网关模式：连接用户 VPS 上的 comfy-colab-kit 网关 |
| `localgate.go`（`//go:embed kit`）、`colab.go`、`colab_auth.go`、`libstore.go`、`local_api.go` | 本地模式（实验）：程序自己当网关，用 Google 账号直接开关 Colab（非公开接口） |
| `jobs.go`、`templates.go`、`labels.go` | 任务队列、ComfyUI API 工作流模板、参数中文名/推荐值 |
| `ai.go`、`llm.go` | AI 页对话（经 ComfyUI 的 `/wbllm` 路由到 VM 上的 Ollama）、AI 写提示词（内置原创的通用说明 `builtinPromptGuide`；用户可导入 H3 官方指南到 `<dataDir>/prompts/h3_guide.md`，有则优先用） |
| `ai_agent*.go`、`ui/ai-agent.js` | Windows 自动执行实验：仅 DeepSeek-R1-70B-Q5/Qwen3.8，用户单独开始；严格 JSON 动作、真实工具结果、暂停补充、取消/重试/容量控制。test9 共用错误恢复、重复阻断和主动求助，真实回答后继续；求助临时替代聊天输入区。文件工具限定目录；PowerShell 非沙盒，独立浏览器 CDP，不是任意桌面应用控制。执行日志/备份只存用户目录，不嵌入 exe |
| `ai_attachments.go`、`ai_skills*.go`、`ai_chat_state.go` | AI 对话附件解析、SKILL.md 与 GitHub 导入、本机持久化；内容存数据目录，绝不把用户附件或导入技能嵌入 exe。图片核对实际视觉能力；技能脚本不执行 |
| `ai_binary.go` | 程序/未知二进制静态报告、原文件磁盘保存及内容探测；不执行上传文件。报告标注结构和字符串采样范围，聊天选段保留“静态报告”标记；失败/取消清理新内容文件 |
| `ai_attachment_store.go`、`ai_attachment_context.go`、`ai_skill_batch.go` | 大附件使用磁盘内容缓存并按问题选段，回复披露实际片段；技能整库批量安装、重试和用户消息点名临时调用。安装全部不等于全部启用 |
| `ai_context.go`、`ai_vision.go`、`ui/ai-vision.js` | 自动打包近期上下文/技能节选，保留完整本机数据；本地已就绪会话可补装匹配视觉组件，按 VM 实际模型名发送。网关补装需独立 VPS helper，不能通过聊天代理执行任意安装命令 |
| `ai_long_context.go`、`gateway/src/long_context.py` | 独立 1M 实验纯文字配置：固定 llama.cpp/YaRN、运行容量确认、模板 token 计数、超限重打包与流式转换；原 Ollama 路径保留。生成器同步到 kit，不手改生成文件。真实 GPU/满窗口效果待验收 |
| `gpu.go` | GPU/系统负载面板 |
| `custom_models.go` | 「添加模型」：解析 HF 链接、读 HF API（走 proxy.go 的 transport）、识别四种类型、推荐 GPU、生成并校验 profile.yaml（`validateProfileYAML` 是 kit/gate/profiles.py `parse_profile` 的 Go 版）；本地网关的 `/_gate/custom-profiles` |
| `ui/models.js` | 「添加模型」弹窗、设置里的自定义模型列表（依赖 app.js / ai.js 的全局函数） |
| `testdata/hf/` | 手工构造的 HF API 响应（测试用） |
| `userdata.go` | 备份 / 恢复 ComfyUI 的 `user/default/`（只用 ComfyUI 自带的 userdata 接口，经 baseURL + addAuth，路径里 `/` 编码成 `%2F`）：本地 `<dataDir>/comfy-user/`（files/、manifest.json、.trash/）；每次开机先恢复一次，恢复成功后才备份和镜像删除；停止前备份最多 20 秒；启动日志行以 `WB_USERDATA ` 开头（网关模式由 server.go 的 sseProxy 插进事件流） |
| `ui/userdata.js` | 控制台的「工作流备份」状态行 |
| `onboarding.go` / `ui/onboarding.js` | 首次使用引导：进度与入门清单存在 config（`onboarded`、`onboard_step`、`checklist`、`cu_rates`，设置表单保存时保留这些字段）、网络检测 `/api/netcheck`、CU/小时估计（`cuRateGuess`，GPU 就绪时记住实际值）；简单路径 3 次点击开机，完整向导 7 步，功能导览气泡 |
| `docs/GETTING_STARTED.md` | 新手指南（与引导步骤对应；截图占位在 `docs/img/`） |
| `testdata/plans/` | 内置启动配置在「core 模型」改动前生成的计划快照，`TestBuiltinPlansUnchanged` 对比 |
| `update.go` | 自动更新：读 GitHub Releases 的 `latest.json`，校验 SHA-256 后替换；`AppVersion` 在这里 |
| `desktop_icon_windows.go` / `winres/refresh-shortcuts.ps1` | 正常启动后迁移当前 EXE 的桌面快捷方式图标并通知 Shell 刷新；保留参数/其他安装副本，headless 不触碰桌面，Windows 原生隔离测试 |
| `*_windows.go` / `*_linux.go` / `*_other.go` | 平台相关（窗口、单实例、系统代理）；Linux 使用浏览器工作台和文件锁 |
| `rsrc_windows_amd64.syso`、`winres/` | 图标/版本资源（go-winres 生成） |
| `ui/` | 内嵌界面：`index.html`、`app.js`、`ai.js`、`style.css` |
| `kit/` | 上传到 Colab 的启动套件（来自 FireTable/comfy-colab-kit，MIT）：`colab/setup.py`（VM 上的启动脚本）、`gate/`（配置/计划解析）、`profiles/`（内置启动配置）、`driver.py`、`wbproxy.py` |
| `gateway/` | 给用户 VPS 网关打的一键补丁：`gate-gpu-select.py`、`gate-library.py`、`gate-ai.py`（生成物） |
| `gateway/src/edits.py` + `build.py` | `gate-ai.py` 的源码；**不要手改 `gate-ai.py`** |
| `.github/workflows/release.yml` | 发版工作流 |

## 构建 / 测试

```bash
cd app
go test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H windowsgui" -o Wakeloom.exe .
```

- 可在 Linux 上测试和交叉编译（Go 1.24.7，见 go.mod）。
- 部分测试需要环境变量（如 `JUPYTER_TEST_URL`），没设会自动跳过。
- `CKWB_TEST_AGENT_BROWSER=1 go test ./...` 启用实际本机 Edge/Chrome 隔离浏览器测试；自动任务测试用模拟模型与临时目录，不接真实 GPU。Windows 命令测试验证取消及后台子进程清理。普通聊天/附件保持不执行；自动任务必须由用户单独启用。
- 调试：`CKWB_HEADLESS=1 [CKWB_PORT=xxxx] go run .` 只起本地服务、打印 URL，不开窗口。UI 测试用的假服务地址：`CKWB_HF_BASE`（Hugging Face API，数据见 `testdata/hf/`）、`CKWB_GOOGLE_TOKEN_URL`（Google 令牌接口）、`CKWB_COLAB_BASE`（Colab `/tun/m` 接口）。空的 `HOME`/`APPDATA` 启动时会自动弹出新手引导。
- `custom_models_test.go` 会调用 python3 + PyYAML 用 kit 的 `parse_profile` 交叉校验 Go 生成的 YAML，并比对 `MODEL_DIRS`；改目录列表或校验规则时两边要同步（Python 侧在 `gateway/src/edits.py` 的 `MODEL_DIRS` / `CUSTOM`）。
- 图标/版本资源改动后：`go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --arch amd64 --out rsrc`

## 修改 kit / 网关补丁（重要）

本地 test8 起，生成器通过 `edits.py` 的 `LOCAL_SETUP` 为内嵌 kit 启用与 `gate-bootstrap.py` 同源的引导重试；VPS 完整补丁仍使用原 `SETUP`，可另行应用独立引导补丁。不要因升级桌面套件而改动现有生产网关。

`kit/colab/setup.py`、`kit/gate/profiles.py`、`kit/gate/plan.py` 中 AI 相关部分（`# ── wb-ai` 区块、`ai_prepare`/`ai_finish`、`wb-boot` 等）由 `gateway/src/edits.py` 生成，**两边必须保持一致**：

1. 改 `gateway/src/edits.py`（`AI_SETUP` 等是 raw 字符串 `r'''…'''`，注意转义）。
2. `python3 gateway/src/build.py` → 重新生成 `gateway/gate-ai.py`。
3. `python3 gateway/gate-ai.py kit` → 同步到内嵌 kit（带区域标记的修改会整段替换）。
4. 删除生成的 `.bak`：`find kit -name '*.bak' -delete`，以及 `__pycache__`。
5. 自检：再跑一次 `python3 gateway/gate-ai.py kit` 应全部显示 `= … up to date`。

网关专用的修改（`apps/gate/app.py`、`session.py`：`/_gate/custom-profiles` 接口与加载 `STATE_DIR/custom_profiles`）在 `edits.py` 的 `APP` / `SESSION` 里，只对网关目录生效，内嵌 kit 没有这两个文件。测试网关补丁：把 FireTable/comfy-colab-kit 克隆到临时目录，依次运行 `gate-gpu-select.py`、`gate-library.py`、旧版和新版 `gate-ai.py`，再在那里跑 `pytest tests/test_wb_custom_profiles.py`。

内置启动配置（Qwen3.8 27B、RealVisXL V5.0、Chroma1-HD）的 profile.yaml 也来自 `edits.py` 的 `PROFILE_*`，步骤 3 会覆盖 `kit/profiles/` 下对应文件。
kit 的日志输出用英文；程序界面和更新日志用中文。

## 发版流程

1. 改 `app/update.go` 的 `const AppVersion = "X.Y.Z"`。
2. 在 `CHANGELOG.md` 顶部（`# 更新日志` 下）加 `## vX.Y.Z` 和若干 `- ` 条目（会成为 Release 说明和 `latest.json` 的 notes）。
3. `go test ./...`，提交并推送到 `main`。
4. 主分支上修改 `app/update.go` 会自动触发 release；也可推标签 `git tag vX.Y.Z && git push origin vX.Y.Z`，或在 Actions 页手动运行 release，发布 `AppVersion` 对应的版本。同一版本只选一个触发方式，避免并行上传。
5. 工作流会：校验标签与 `AppVersion` 一致（不一致直接失败）→ 测试 → 构建 `Wakeloom-X.Y.Z.exe` → 生成 `latest.json` → 连同 `gateway/gate-*.py` 发布到 GitHub Release。程序的自动更新读 `releases/latest/download/latest.json`。

## 注意事项

- 不要提交 exe / `dist/` / `__pycache__` / `.bak`（见 `.gitignore`）。
- 本地模式依赖 Colab 非公开接口和 Google Cloud SDK 的公开 OAuth 客户端，改动时保持与 google-colab-cli 行为一致。
- 1.17 改名前的数据目录是 `ComfyColabWorkbench`，`config.go` 会自动迁移；环境变量前缀仍是 `CKWB_`、module 名仍是 `ckwb`，不要随意改。
- 自定义模型的保存规则（Go 的 `customProblem` 与网关 Python 的 `custom_problem` 必须一致）：至少有一个会被下载的模型、要么有 `ai` 块要么有 `wb_entry.template`、总大小 ≤ 86.4 GB；「任意模型」文件标 `core: true`，plan.py 只把**没有任何工作流 / smoke / ai 引用**的 core 模型加进计划（这样内置配置的计划不变）。
- 用户数据同步的请求带 `X-WB-Sync` 头：本地网关不把它算作活动；VPS 网关没有这种开关，会算作活动，所以定时同步间隔是 5 分钟且只在就绪时运行。
- VM 上 subprocess 输出要经 Python `print` 转发（见 setup.py 的 `sh()`），直接写 fd 1 不会出现在唤醒日志里；长时间步骤要定期打印进度，否则看起来像卡住。
