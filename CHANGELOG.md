# 更新日志

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
