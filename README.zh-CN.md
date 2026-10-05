# SudoGate

**反向 askpass** —— 远程主机上 AI agent 的 sudo 密码框，弹回你面前这台机器。

[English](README.md) · [简体中文](README.zh-CN.md)

## 为什么

在远程主机上跑 coding agent（opencode / Claude Code / codex……）迟早要 `sudo`。
常见选择都不理想：

- **sudoers 免密** —— agent 跑的一切代码从此握着 root
- **把密码贴进对话** —— 密码进入模型上下文与转录
- **转发 GUI 弹窗** —— 依赖转发显示，headless 主机不可用

SudoGate 换了一条路：agent 照常 `sudo -A`，密码框沿着**你已经开着的 ssh 会话**
弹回你的 ssh 客户端机器。每次提权都由人读命令、人输密码；密码每次现输，
两端零落盘。

## 特性

- 每命令人在环上 —— 一次提权一次审阅，拒绝即刻终止
- 密码零落盘 —— 两端不存储任何凭据
- 不新增信任面 —— 通道即 ssh `-R` unix socket 反向转发，认证与加密由 ssh
  自带；不开端口、不配证书
- 密钥编译期拆分 —— server 二进制内嵌私钥、client 内嵌公钥，代码中不存在
  密钥生成路径
- 结构性防篡改 —— 每请求 X25519 一次性密封 + Ed25519 签名 + 一次性 id 与
  命令哈希绑定
- 断线即拒绝 —— ssh 不在线，远程 `sudo -A` 直接失败
- 审计 —— 每次决断记 JSONL
- 与 agent 自身闸门组合 —— 如 opencode 的 `"*sudo*": "ask"`，每条命令两道
  独立审批

## 快速开始

依赖：Go ≥ 1.24、make、openssl；传统 sudo（sudo-rs 不支持 askpass）。

一次性生成密钥对（任意第三方工具），带外保管——仓库永不存密钥，每台
参与构建的机器使用同一对：

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/laptop.key
openssl pkey -in keys/laptop.key -pubout -out keys/laptop.pub
```

**ssh 客户端机器**（你发起 ssh 的那台）：

```bash
make install-server    # 构建 + ~/.local/bin + systemd --user 服务
make install-plugin    # omarchy 审阅面板（可选，推荐）

# ~/.ssh/config:
Host minipc
    RemoteForward /run/user/1000/sudogate.sock /run/user/1000/sudogate.sock
    ExitOnForwardFailure yes
```

**远程主机**（agent 跑的那些机器）：

```bash
make install-client    # askpass 二进制装入 ~/.local/bin
# 交互 shell：
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
# agent（非交互）环境继承需另配 —— 见「与 agent 协作」
sudogate-client test   # 打印内嵌公钥指纹
```

之后在 agent 或 shell 里：`sudo -A <命令>` —— 请求出现在 ssh 客户端机器上
等待审阅。

**审阅界面** —— 两种方式，同一个 server：

- **omarchy 面板**（`make install-plugin` 安装）：状态栏钥匙徽标（🔑 + 待批数）
  ——有待批时红色醒目，空闲时灰色暗淡（钥匙 + 0）。**默认徽标常驻栏位**，
  一眼可见面板存活；配置 `hideWhenEmpty` 可改为空闲时完全隐藏。点击展开
  审阅卡——主机/用户/目录/完整命令/倒计时，密码框（回车 = 批准）与拒绝
  按钮。到期请求自动滚动并清空密码框。随时
  `git pull && make install-plugin` 升级（幂等安装）。
- **CLI 兜底**（任意环境）：

  ```bash
  sudogate-server status   # 查看待批
  sudogate-server review   # 审阅最旧一条：显示命令，隐藏输入密码；
                           #   输密码回车 = 批准，直接回车 = 拒绝
  ```

**面板配置** —— 写在 `~/.config/omarchy/shell.json` 布局条目上，改动即时
生效（无需重启 shell）：

```jsonc
{ "id": "tomaswade.sudogate", "hideWhenEmpty": true, "timeoutSec": 120 }
```

| 选项 | 默认 | 含义 |
|---|---|---|
| `hideWhenEmpty` | `false` | `false`：徽标常显（空闲时灰钥匙 + 0）；`true`：队列空时徽标完全隐藏 |
| `timeoutSec` | `120` | 每请求等待上限，驱动倒计时显示；与 server 的 `-timeout` 保持一致 |
| `runtimeDir` | `""` | `sudogate.sock.ctl` / `sudogate.state` 所在目录；空 = `XDG_RUNTIME_DIR` |
| `serverBin` | `""` | 批准/拒绝调用的 `sudogate-server` 路径；空 = 自动探测（优先 `~/.local/bin`） |
| `demo` | `false` | 用假数据渲染（无需 server），预览视觉用 |

## 与 agent 协作

SudoGate 是**第二道**闸门。第一道在 agent 自己的权限配置里；两道合起来，
每次提权经过两次独立的人工审批。

**闸门 1 —— agent 跑 sudo 之前先问你。** opencode 的配法：

```jsonc
// ~/.config/opencode/opencode.json
{
  "permission": {
    "bash": {
      "*sudo*": "ask"
    }
  }
}
```

Claude Code / codex 有各自等价物；任何能对 `sudo` 命令强制人工确认的工具
都可以当闸门 1。

**闸门 2 —— 密码出发之前 SudoGate 再问一次。** 完整流程：

```
agent 需要 root
  → 闸门 1：agent 工具弹「要跑 sudo -A …吗？」—— 你批准这个意图
  → agent 执行 sudo -A <命令>
  → sudogate-client 沿你的 ssh 会话转发请求
  → 闸门 2：你机器上的钥匙徽标变红；打开面板，
    读主机 / 用户 / 目录 / 完整命令
  → 命令与闸门 1 刚批的对得上？输密码回车。对不上 —— 拒绝。
  → sudo 执行（或中止）；agent 只看到退出码
```

让钓鱼难以得手的审阅纪律：**只批准命令与闸门 1 刚批内容对得上的请求**。
你没批准过的命令、你没有会话的主机 —— 一律拒绝。

**接好 agent 的环境。** `sudo` 从自身环境读 `SUDO_ASKPASS`，而该环境继承自
agent 进程。agent 的 shell 是非交互的：**不读任何 rc 文件**，所以快速开始里
写入 `~/.profile` 的导出只覆盖交互会话。变量必须进入 agent 进程环境：

- agent 从交互 shell 启动 —— 继承该 shell 的环境。把导出放在 `~/.bashrc`
  的非交互 guard **之前**（zsh 放 `~/.zshenv`），所有子进程都能拿到：

  ```bash
  # ~/.bashrc 首行 —— 必须在 "case $- in *i*)" guard 之前
  export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client
  ```

- agent 由 systemd（或其他监管器）启动 —— 写进 unit：

  ```ini
  [Service]
  Environment=SUDO_ASKPASS=%h/.local/bin/sudogate-client
  ```

在 agent 自己的 shell 工具里验证：

```bash
echo "${SUDO_ASKPASS:-MISSING}"
```

**agent 看到什么** —— `sudo -A` 会阻塞到出决断：

| 操作者的动作       | agent 看到                          |
|--------------------|-------------------------------------|
| 输了密码           | 命令以 root 执行，正常退出          |
| 拒绝               | exit 1，stderr 带 askpass 错误      |
| 超时（默认 120s）  | exit 1，stderr 带 timeout 错误      |

等待 = 有个人正在读你的命令——agent 应把阻塞当作正常现象，不要对未决请求
重试轰炸（相同的并发命令本来就会合并为一次审阅）。

## 架构

```
远程主机（agent）                     ssh 客户端机器（操作者）
───────────────                      ──────────────────────
sudo -A <cmd>
 └─ sudogate-client（= SUDO_ASKPASS）
     · ps 取证真实命令
     · 一次性 X25519 密钥对
           │ ── ssh -R unix socket ──▶ sudogate-server（systemd --user）
           │    请求 {命令, id, 公钥}          │ 审阅 UI：omarchy 面板或 CLI
           │                                  │ 操作者输入密码
           ◀─ 密封密码 + 签名 ────────────────┘
 └─ 验签（编译内嵌公钥）
     一次性密钥解封 → stdout → sudo
```

- **组件** —— `sudogate-server`（ssh 客户端机器）：监听由各远程主机转发来
  的 unix socket，排队请求、密封签名响应；以 systemd --user 服务常驻；内嵌
  私钥。`sudogate-client`（远程主机）：askpass helper；内嵌公钥、别无秘密，
  由 sudo 按需调起、跑完即退，可复制到任意多台主机。`plugin/`（ssh 客户端
  机器）：omarchy quickshell 审阅面板——inotify 监听 server 状态文件（零轮询），
  经 server 控制子命令批准/拒绝，密码走 stdin。
- **信任与密码学** —— 传输层认证/加密继承自 ssh；响应来源由 Ed25519 对
  `id ‖ SHA256(命令) ‖ SHA256(公钥)` 的签名证明，验签用编译内嵌公钥；密码以
  XChaCha20-Poly1305 在 X25519/HKDF 密钥下密封，只有等待中的 askpass 进程
  能打开。
- **已知边界** —— 远程主机上同 uid 代码可自行提交请求（钓鱼级；防线是人读
  命令，与本地桌面弹框同级）；同 uid DoS 不可避免；ssh 客户端机器失陷即
  全盘失陷（私钥在此）。

## 行为（实测）

| 你的操作     | 弹框            | 结果                     |
|--------------|-----------------|--------------------------|
| 输对密码     | 1 次            | 以 root 执行             |
| 输错密码     | 至多 3 次       | sudo 放弃                |
| 直接拒绝     | 仅 1 次         | 立即终止                 |
| 空输入       | 仅 1 次         | 视为拒绝                 |

并发请求各自独立密封与决断；相同的并发命令自动合并为一次审阅。

## 排障

- **`sudo：没有指定 askpass 程序`** —— `SUDO_ASKPASS` 没到 sudo 进程。agent
  的 shell 非交互、不读 rc 文件；环境链条见[「与 agent 协作」](#与-agent-协作)。
- **`remote port forwarding failed for listen path …`** —— 上次会话残留的
  `/run/user/1000/sudogate.sock` 挡住了转发，`ExitOnForwardFailure` 随即断连。
  用免转发的一次性命令清掉：

  ```bash
  ssh -o ClearAllForwardings=yes minipc 'rm -f /run/user/1000/sudogate.sock'
  ```

- **人不在时 `sudo -A` 失败** —— 这就是设计（断线即拒）；桥只存在于你活着
  的 ssh 会话里。
- **sudo-rs** —— 完全不支持 askpass；请用传统 sudo（sudo.ws 的 `sudo`）。
- **`sudo -n true` 仍要密码** —— 正确：SudoGate 刻意不提供任何免密路径，
  sudoers 无需任何配置。

## 参考

- [lllamnyp/askpass](https://github.com/lllamnyp/askpass) —— 同模式，mTLS 传输
- [OzoneAsai/remote-askpass](https://github.com/OzoneAsai/remote-askpass) —— Windows 侧变体
- [crypdick/sudoplz](https://github.com/crypdick/sudoplz) —— SSH 密钥加密存储型 askpass
- [0xMH/sudo-mcp](https://github.com/0xMH/sudo-mcp) —— MCP 工具弹本机 OS 密码框（agent 与人须同机）
- [hughesjs/sudo-mcp](https://github.com/hughesjs/sudo-mcp) —— MCP + polkit/pkexec（依赖本机桌面会话）

## 许可证

[MIT](LICENSE) — Copyright (c) 2026 tomasWade
