# Linux 版本

Linux 版与 Windows 版共用工作台、模型配置、任务队列、附件、技能、本地直连和 VPS 网关功能。界面在默认浏览器打开；云端仍负责运行模型，本机无需安装 CUDA、Python、Ollama 或 WebView2。

Linux 源码和本地测试构建暂不发布下载或自动更新清单，等待用户实机测试。CI 检查不代表真实 Linux 桌面、Google 登录和 Colab 唤醒已验收。下面的启动说明供本地测试使用。

## 启动

普通 Intel/AMD 64 位电脑选择 `linux-amd64.tar.gz`，ARM64 电脑选择 `linux-arm64.tar.gz`。解压后在该目录打开终端：

```sh
./wakeloom
```

如果下载的是单个二进制，先执行 `chmod +x 文件名`，再用 `./文件名` 启动。不要用 sudo 运行。程序只监听 `127.0.0.1`，终端会显示带访问密钥的本机网址；桌面系统使用 xdg-open 或 gio 自动打开浏览器，失败时复制终端网址到浏览器。

关闭浏览器标签页不会结束任务。退出前在工作台停止云端 GPU，再回终端按 Ctrl+C。终止本机进程不会自动释放云端 GPU。

## 数据与代理

- 数据默认保存在 `~/.config/Wakeloom`；设置绝对路径 `XDG_CONFIG_HOME` 后使用其下的 `Wakeloom`。旧 `ComfyColabWorkbench` 目录自动迁移。
- 默认结果目录为 `~/WakeloomOutputs`，可以在设置里修改。
- 左侧「日志」可预览、复制或导出诊断信息，文件保存在数据目录的 `logs/`；常见凭据自动过滤，分享前请检查预览。
- 代理可在软件设置里填写，也支持 `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` 环境变量。
- 一个配置目录同时只能运行一个实例，包括无界面模式；异常退出后可直接重开。

## 无桌面使用

```sh
CKWB_HEADLESS=1 CKWB_PORT=5826 ./wakeloom
```

程序仍只监听本机。如果在远程 Linux 上运行，通过 SSH 端口转发访问：`ssh -L 5826:127.0.0.1:5826 用户@主机`，然后在本机浏览器打开程序输出的网址。Google 登录时可手动打开界面给出的授权链接。请保持进程和终端会话运行。

## 构建与更新

使用 go.mod 指定的 Go 版本或兼容版本，以及 Python 3 打包：

```sh
go -C app test ./...
python3 scripts/build-linux.py
python3 scripts/smoke-linux.py dist/Wakeloom-1.21.4-linux-amd64
```

输出含两个架构的静态二进制、tar.gz、SHA-256 和独立更新清单。Linux 自动更新按架构选择 `latest-linux-amd64.json` / `latest-linux-arm64.json`，校验摘要并保留执行权限；拒绝 Windows 清单和其他架构。正式清单发布前，检查更新可能提示 404，可继续使用测试包。程序目录需可写才能自动更新。

Windows 用户的旧 `latest.json` 格式继续兼容。若搬迁 Windows 数据，请先备份并调整输出目录、代理及更新地址，不能直接沿用 Windows 路径。
