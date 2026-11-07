# 新手指南

第一次用 Wakeloom（唤织）？跟着这一页，从下载到生成第一张图大约 15 分钟，其中一半时间是等 GPU 开机。

软件第一次打开时会自动弹出引导，步骤和这一页一一对应。关掉后可以从右上角的「?」→「新手引导」或「设置 → 软件更新 → 重新运行新手引导」再打开。

---

## 1. 准备什么

| | 说明 |
|---|---|
| Windows 10 / 11 | 需要系统自带的 WebView2（Windows 11 和新版 Windows 10 都有） |
| Google 账号 + Colab 付费算力（CU） | Colab Pro / Pro+ 订阅，或[按量购买](https://colab.research.google.com/signup)的计算单元。免费版只有 T4，跑不了大模型。[什么是 CU？](https://research.google.com/colaboratory/faq.html#compute-units) |
| 网络 | 要能访问 Google、Hugging Face、GitHub。国内需要开代理；软件会自动走系统代理（Clash、v2rayN 的「系统代理」模式） |

![第 1 步：欢迎](img/step1.png)

## 2. 下载和第一次运行

1. 到 [Releases](../../../releases/latest) 下载 `Wakeloom-x.y.z.exe`。
2. 双击运行。软件没有代码签名，Windows 可能弹出「Windows 已保护你的电脑」（SmartScreen）：
   - 点 **更多信息**；
   - 再点 **仍要运行**。

   ![SmartScreen：更多信息 → 仍要运行](img/smartscreen.png)

3. 想确认文件没被篡改，可以核对 SHA-256（Release 页的 `latest.json` 里有 `sha256`）。在下载目录打开 PowerShell：

   ```powershell
   Get-FileHash .\Wakeloom-1.21.0.exe -Algorithm SHA256
   ```

   或者用命令提示符：`certutil -hashfile Wakeloom-1.21.0.exe SHA256`。两边的值应该完全一样。

## 3. 最简单的方式：三步启动 Colab

引导默认走这条路，不需要服务器，也不用填表。

1. 点 **用 Google 账号开始**。软件会切到「本地模式」，并在浏览器里打开 Google 授权页。
2. 在浏览器里登录、点「允许」，页面会显示一串授权码，点旁边的「复制」。回到软件，按钮会变成 **检测到授权码 · 一键登录**，点它。读不到剪贴板时，展开「手动粘贴」按 Ctrl+V。

   ![第 2 步：登录 Google](img/step2.png)

3. 登录后会显示 CU 余额和推荐组合「RealVisXL 生图 · L4」。点 **启动 Colab**。

   ![第 3 步：一键开机](img/step3.png)

软件会跳到「AI」页的生图面板，并填好一个示例提示词。右下角的小窗显示开机进度：第一次要下载约 7 GB 模型，大约 5～8 分钟。就绪后点 **生成**，几十秒就能看到第一张图。

> 本地模式是实验功能：它用的是 Colab 的非公开接口和 Google Cloud SDK 的公开登录客户端，接口可能随时变化，也有账号被限制的风险。长期稳定使用推荐网关模式。

## 4. 我有自己的网关

已经在 VPS 上部署了 [comfy-colab-kit](https://github.com/FireTable/comfy-colab-kit) 网关的话，在欢迎页点「我有自己的网关 →」，按完整步骤走：

1. **检查网络**：逐项检测 Google、Colab、Hugging Face、GitHub，显示耗时和当前代理。
2. **选择连接方式**：选「网关模式」。
3. **填写网关**：网关地址、Agent 令牌（网关网页右上角「复制 AI Agent 接入指令」里，或 VPS 上的 `secrets/agent_token`），网关在 Cloudflare Access 后面时再填服务令牌。点「测试连接」，成功后会显示网关上的启动配置数量。
4. 网关要打补丁才支持选 GPU、AI 页和添加模型，见 [gateway/README.md](../gateway/README.md)。

![第 4 步：连接网关](img/step4-gateway.png)

## 5. 基本设置

- **结果保存位置**：默认「桌面\生成视频」，生成的文件会自动下载到这里。
- **空闲自动停机**：本地模式默认 30 分钟没人用就停机。GPU 开着就一直扣 CU，这一项就是为了省 CU。
- **大模型 API**（「AI 写提示词」用）：可以跳过，以后在「设置 → 大模型 API」里添加。

## 6. 怎么省 CU

- **用完就点「停止」**。GPU 开着就按小时扣费，哪怕什么都没做。
- 选**够用的最小 GPU**：生图（SDXL）用 L4 就够；大语言模型、Chroma 这类大模型才需要 G4。唤醒按钮旁边会显示「预计每小时约 X CU」：用过一次后按 Colab 实际报的值显示，之前是估计值。
- 打开「设置 → 任务 → 队列全部完成 3 分钟后自动停止 GPU」。
- 本地模式把「闲置自动停机」设短一点；网关模式在网关的 `.env` 里设置。
- 先用「低清试生成」看效果，满意再出高清。

## 7. 常见问题

**申请不到 GPU / 一直在「分配机器」**
Colab 高峰期可能暂时没有空闲的 GPU。换一个型号（比如 G4 换 L4），或过几分钟再试。确认 CU 余额大于 0。

**网络不通（检查网络里有 ✗）**
打开代理软件的「系统代理」模式；或在「设置 → 网关连接 → 网络代理」里填代理地址，例如 `http://127.0.0.1:7890`（按你代理软件的端口）。只有 Hugging Face 不通时，「添加模型」用不了，但开机下载模型是在 Colab 上进行的，不受影响。

**H100 没有空闲**
H100 经常被占满。大多数模型用 G4（96 GB 显存）就够，而且更容易申请到。

**显存不够（out of memory）**
换更大的 GPU（L4 → G4），或者换更小的模型 / 量化版本（比如 Q8_0 换 Q4_K_M），或者把生图尺寸调小。「添加模型」会按「文件大小 × 1.2」估算显存，超过 96 GB 的模型不能保存。

**工作流没了**
Colab 每次开机都是新机器。软件默认会把 ComfyUI 里保存的工作流和设置备份到电脑上，下次开机自动传回去（控制台「工作流备份」一行）。

---

还有问题？看 [README](../README.md) 或提 [Issue](../../../issues)。
