# CLAUDE.md — Wakeloom（唤织）维护说明

Windows 桌面程序：按需唤醒 Colab GPU 跑 ComfyUI 生成视频/图片，AI 页用 Ollama 跑 GGUF 大模型对话 + SDXL/Chroma 生图，空闲自动停机。
单文件 exe（Go + WebView2），界面、启动套件、提示词全部 `go:embed` 进 exe。

## 目录结构

| 路径 | 内容 |
|---|---|
| `*.go`（module `ckwb`，package main） | 桌面程序：本地 HTTP 服务 + WebView2 窗口 |
| `main.go` | 入口：单实例、数据目录、配置、本地网关、任务 worker、本地 HTTP 服务，再开窗口 |
| `server.go` | 本地 HTTP API，`//go:embed ui` |
| `gate.go` / `proxy*.go` | 网关模式：连接用户 VPS 上的 comfy-colab-kit 网关 |
| `localgate.go`（`//go:embed kit`）、`colab.go`、`colab_auth.go`、`libstore.go`、`local_api.go` | 本地模式（实验）：程序自己当网关，用 Google 账号直接开关 Colab（非公开接口） |
| `jobs.go`、`templates.go`、`labels.go` | 任务队列、ComfyUI API 工作流模板、参数中文名/推荐值 |
| `ai.go`、`llm.go`（`//go:embed prompts/h3_guide.md`） | AI 页对话（经 ComfyUI 的 `/wbllm` 路由到 VM 上的 Ollama）、AI 写提示词 |
| `gpu.go` | GPU/系统负载面板 |
| `custom_models.go` | 「添加模型」：解析 HF 链接、读 HF API（走 proxy.go 的 transport）、识别四种类型、推荐 GPU、生成并校验 profile.yaml（`validateProfileYAML` 是 kit/gate/profiles.py `parse_profile` 的 Go 版）；本地网关的 `/_gate/custom-profiles` |
| `ui/models.js` | 「添加模型」弹窗、设置里的自定义模型列表（依赖 app.js / ai.js 的全局函数） |
| `testdata/hf/` | 手工构造的 HF API 响应（测试用） |
| `update.go` | 自动更新：读 GitHub Releases 的 `latest.json`，校验 SHA-256 后替换；`AppVersion` 在这里 |
| `*_windows.go` / `*_other.go` | 平台相关（窗口、单实例、系统代理）；非 Windows 版本只是桩，便于 Linux 上测试 |
| `rsrc_windows_amd64.syso`、`winres/` | 图标/版本资源（go-winres 生成） |
| `ui/` | 内嵌界面：`index.html`、`app.js`、`ai.js`、`style.css` |
| `prompts/` | AI 写提示词的系统提示 |
| `kit/` | 上传到 Colab 的启动套件（来自 FireTable/comfy-colab-kit，MIT）：`colab/setup.py`（VM 上的启动脚本）、`gate/`（配置/计划解析）、`profiles/`（内置启动配置）、`driver.py`、`wbproxy.py` |
| `gateway/` | 给用户 VPS 网关打的一键补丁：`gate-gpu-select.py`、`gate-library.py`、`gate-ai.py`（生成物） |
| `gateway/src/edits.py` + `build.py` | `gate-ai.py` 的源码；**不要手改 `gate-ai.py`** |
| `.github/workflows/release.yml` | 发版工作流 |

## 构建 / 测试

```bash
go test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H windowsgui" -o Wakeloom.exe .
```

- 可在 Linux 上测试和交叉编译（Go 1.24.7，见 go.mod）。
- 部分测试需要环境变量（如 `JUPYTER_TEST_URL`），没设会自动跳过。
- 调试：`CKWB_HEADLESS=1 [CKWB_PORT=xxxx] go run .` 只起本地服务、打印 URL，不开窗口。`CKWB_HF_BASE=http://127.0.0.1:端口` 可把 Hugging Face API 指到本地假服务（UI 测试用，数据见 `testdata/hf/`）。
- `custom_models_test.go` 会调用 python3 + PyYAML 用 kit 的 `parse_profile` 交叉校验 Go 生成的 YAML，并比对 `MODEL_DIRS`；改目录列表或校验规则时两边要同步（Python 侧在 `gateway/src/edits.py` 的 `MODEL_DIRS` / `CUSTOM`）。
- 图标/版本资源改动后：`go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --arch amd64 --out rsrc`

## 修改 kit / 网关补丁（重要）

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

1. 改 `update.go` 的 `const AppVersion = "X.Y.Z"`。
2. 在 `CHANGELOG.md` 顶部（`# 更新日志` 下）加 `## vX.Y.Z` 和若干 `- ` 条目（会成为 Release 说明和 `latest.json` 的 notes）。
3. `go test ./...`，提交并推送到 `main`。
4. 推标签：`git tag vX.Y.Z && git push origin vX.Y.Z`（也可在 Actions 页手动运行 release，发布 `AppVersion` 对应的版本）。
5. 工作流会：校验标签与 `AppVersion` 一致（不一致直接失败）→ 测试 → 构建 `Wakeloom-X.Y.Z.exe` → 生成 `latest.json` → 连同 `gateway/gate-*.py` 发布到 GitHub Release。程序的自动更新读 `releases/latest/download/latest.json`。

## 注意事项

- 不要提交 exe / `dist/` / `__pycache__` / `.bak`（见 `.gitignore`）。
- 本地模式依赖 Colab 非公开接口和 Google Cloud SDK 的公开 OAuth 客户端，改动时保持与 google-colab-cli 行为一致。
- 1.17 改名前的数据目录是 `ComfyColabWorkbench`，`config.go` 会自动迁移；环境变量前缀仍是 `CKWB_`、module 名仍是 `ckwb`，不要随意改。
- VM 上 subprocess 输出要经 Python `print` 转发（见 setup.py 的 `sh()`），直接写 fd 1 不会出现在唤醒日志里；长时间步骤要定期打印进度，否则看起来像卡住。
