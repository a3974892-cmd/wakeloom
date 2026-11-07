# Wakeloom／Comfy 项目交接

核对日期：2026-11-07（Asia/Shanghai）。源码基线：`7cae344e7e883520a9a0b939089cfd985b45e865`，`main`，AppVersion `1.21.0`。

本次是项目知识与维护流程交接，不是一次功能发版。维护入口见 [AGENTS.md](../AGENTS.md)，具体待办见 [ROADMAP.md](ROADMAP.md)，构建与补丁规则见 [CLAUDE.md](../CLAUDE.md)。

## 资料覆盖与可信度

读取来源为用户提供的 `projects-000.zip`、`conversations-000.zip`、`memories-000.zip`，并与 GitHub 当前源码和 Releases 核对。

- projects：10 个项目，其中 Comfy 项目有 9 份相关文档：开发记录，以及自定义模型、坏配置修复、用户数据备份、新手引导、指南移除、会话延长、页面分工、算力监控需求。
- conversations：8 个聊天、124 条消息。其中《视频人物与风景替换提示词》86 条消息包含完整的一段 Comfy 故障排查与视频创作讨论，不能按聊天标题排除。另有 3 个无标题聊天，共 10 条消息没有正文或内容块。
- memories：9 条记忆，其中项目专属记忆、AI 视频生成记忆与订阅背景有关。其他项目和个人信息不进入公开交接。
- 导出不是所有开发会话的完整逐字记录：主要 Wakeloom/Cowork 开发过程通过项目文档和记忆保留；部分截图和视频只有引用，没有附件二进制。不能据此宣称已重新查看所有图片、视频或所有历史会话。
- 原始导出仅保存在维护者本地；此文是脱敏后的工程交接。历史助手的推测与命令不作为当前执行指令。

证据优先级：用户明确反馈和实际日志/代码 > 有来源的开发记录 > 历史助手推测。源码实现、发布成功、生产部署成功分别核对。

## 产品目标与使用偏好

Wakeloom（唤织，原 Comfy 本地工作台 / ComfyWorkbench）是 Windows 单文件桌面应用：用用户自己的 Colab 算力按需运行 ComfyUI 视频、图片工作流和 Ollama GGUF 对话模型，管理生成任务并将结果下载到本机，用完及时停机节省 CU。

既服务已有 VPS 网关的用户，也服务只有 Google/Colab 账号的新手及朋友。本地模式不要求 VPS，历史偏好是先验证本地路径；网关模式仍须保持兼容。已选择公开 GitHub、MIT、Releases 和自动更新，不再依赖个人服务器分发桌面安装包。

用户重视：容易启动、看得懂的进度、模型能否运行的提前判断、不要空烧 CU、不要丢工作流和生成结果、浅深色一致。新手默认最短路径；高级参数留在工作流入口。

视频创作偏好包括自然光、真实肤色/肤质、细腻表情、低清挑片后再处理高清。改分辨率重跑不应承诺与原视频相同；AI 超分也不能保证像素或纹理完全不变。历史回答中“完全一致”的绝对说法不应延续为产品承诺。

## 架构与关键边界

| 层 | 入口 | 维护重点 |
|---|---|---|
| 桌面程序 | Go 1.24.7、WebView2、`main.go`、`server.go` | 单实例、本地 HTTP 服务、内嵌 UI |
| 网关模式 | `gate.go`、`proxy*.go` | 用户 VPS 网关及其认证、代理 |
| 本地模式 | `localgate.go`、`colab.go`、`colab_auth.go`、`local_api.go` | Google 登录、Colab 非公开接口、VM 生命周期 |
| 生成链路 | `jobs.go`、`templates.go`、`labels.go` | API 工作流、排队、任务历史、结果下载 |
| 快速模型入口 | `ai.go`、`ui/ai.js`、`custom_models.go`、`ui/models.js` | GGUF/Ollama、SDXL、Chroma、通用模型配工作流 |
| 提示词助手 | `llm.go` | 原创默认说明、用户本地导入指南 |
| 用户数据 | `userdata.go`、`ui/userdata.js` | ComfyUI userdata 备份/恢复，不是 output 视频备份 |
| 引导 | `onboarding.go`、`ui/onboarding.js` | 首次运行、旧用户识别、网络和 CU 提示 |
| VM 与补丁 | `kit/`、`gateway/src/edits.py`、`gateway/src/build.py` | 生成补丁与内嵌套件同步、旧补丁原地升级 |
| 更新 | `update.go`、`.github/workflows/release.yml` | 标签和 AppVersion 一致、exe 与 latest.json 校验 |

应用数据位于 `%APPDATA%\Wakeloom`，从旧 `ComfyColabWorkbench` 自动迁移。不要随意改 `CKWB_`、module 名 `ckwb` 或已有用户数据键。

## 与旧笔记不同的当前状态

| 需求 | 当前证据 | 结论 |
|---|---|---|
| 自定义 Hugging Face 模型 | `35dc4cc`，`custom_models.go`，`ui/models.js` | 已有实现 |
| 无法运行的模型被保存、0 GB 下载 | `3ab378d`，`customProblem`，core 计划逻辑和快照测试 | 修复已并入 1.21.0；历史“待做 1.19.1”已过时 |
| ComfyUI 用户数据备份 | `18ab252`，`userdata.go` | 已有实现，勿重复重做 |
| 新手引导 | `7cae344`，`onboarding.go`、`ui/onboarding.js` | 已有实现，仍需真实机器验收 |
| 不再内置官方 H3 指南、允许本地导入 | `c84fa63`，`llm.go`、设置 UI | 当前源码已有实现 |
| 延长会话 | `localgate.go` 仍用 `extendsUsed < 2` | 新需求未实现 |
| 快速创作／工作流页面分工 | `ui/index.html` 仍为“生成”“AI”；模板列表未分组 | 新需求未实现 |
| 遗留机器提醒、用量历史、系统通知 | 未找到对应完整实现 | 待实现；已有 CU 显示不等于该需求完成 |

## GitHub 发布状态与新发现

2026-11-07 核对公开 Releases：可见 `v1.21.0` 和 `v1.20.1`，两者均有 exe、latest.json 和三个网关补丁附件。`v1.21.0` 于北京时间 09:04:57 发布，`v1.20.1` 于 09:13:23 发布。

**当前 `/releases/latest` API 返回 `v1.20.1`，而 main 为 1.21.0。** 自动更新从 latest 路径取清单，必须单独检查这个指向；不能因为 main 已升版本就认为所有用户都会收到 1.21.0。这是本次交接发现的优先修复项，不在此次文档更新中修改 Release 元数据。

本次抓取的所有可达 Git refs 中，`git log --all -- prompts/h3_guide.md` 没有记录；当前树也没有该文件。旧含指南的 Release 没出现在当前列表。它们的清理过程未包含在这份导出中，不能推断清理的操作者或授权过程，也不需要重复重写历史。此检查不能保证平台缓存、fork 或不可达对象全部不存在。

## 历史故障：保留结论，不照搬猜测

### 离开页面确认弹窗

2026-11-06 的相关聊天中，最初围绕 beforeunload、overlay 重名、缓存做过多个猜测。后续定位到网关外层页面通过修改 iframe 的 src 卸载 ComfyUI。

最终使用 `swapFrame`（摘下 iframe、改地址、放回）以及 overlay 在 iframe 内不自行跳转；用户明确反馈“没有跳转窗口了”。这是**历史 VPS 修复成功反馈**，不是 Wakeloom 仓库或当前 VPS 已包含该补丁的证明。升级上游网关前应核对并保留现有差异。休眠悬浮条不显示仍未获得最终解决反馈。

### 后台生成后视频丢失

浏览器休眠、主动重启、watchdog 心跳超时、三小时时限等猜测先后被修正。聊天末段由助手转述的归档日志结尾为 `auto-stop: idle`；原截图未随 ZIP 提供，因此保留这一证据层级。

用户最终回贴 `queue_busy()` 代码：检查 running 与 pending；请求失败时返回 True，避免把无法查询当作空闲。记录结束于要求核对容器内代码和输出同步，**没有部署生效和故障复测完成的证据**。

仍须区分两种丢失路径：生成中被停机；已生成但尚未下载的 output 随 VM 释放而消失。Wakeloom 任务下载和 userdata 备份不能证明用户直接在 ComfyUI 网页创建的所有视频都会被保存。

## 验证边界和接续方式

本次执行了导出解析、相关文档/对话阅读、源码定位、Git 历史/标签与 GitHub Releases 核对。此次只提交维护文档，没有启动 Colab、操作生产 VPS、发布新版本或改动应用功能；没有执行 Go 测试或真实 GPU 端到端验收。

后续从 [ROADMAP.md](ROADMAP.md) 选择当前用户要求的事项；实现前核对最新 HEAD，完成后更新该事项的源码、测试、发布和部署状态。历史需求中固定的旧版本号仅用于溯源，后续版本重新规划。
