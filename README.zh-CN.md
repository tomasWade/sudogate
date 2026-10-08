# SudoGate

**反向 askpass** —— ssh 服务器上 AI agent 的 sudo 密码框，弹回你面前这台机器。

[English](README.md) · [简体中文](README.zh-CN.md)

## 为什么

在 ssh 服务器上跑 coding agent（opencode / Claude Code / codex……）迟早要 `sudo`。
常见选择都不理想：

- **sudoers 免密** —— agent 跑的一切代码从此握着 root
- **把密码贴进对话** —— 密码进入模型上下文与转录
- **转发 GUI 弹窗** —— 依赖转发显示，headless 主机不可用

SudoGate 换了一条路：agent 照常 `sudo -A`，密码框沿着**你已经开着的 ssh 会话**
弹回你的 ssh 客户端。每次提权都由人读命令、人输密码；密码每次现输，
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
- 断线即拒绝 —— ssh 客户端（server 所在机）关机/失联，转发随 server 消亡，远程 `sudo -A` 直接失败
- 审计 —— 每次决断记 JSONL
- 与 agent 自身闸门组合 —— 如 opencode 的 `"*sudo*": "ask"`，每条命令两道
  独立审批

## 快速开始

先认清三个角色：**构建机**（有仓库和密钥，跑 `make`）、**ssh 客户端**
（你的机器，跑 sudogate-server 和审阅界面）、**ssh 服务器**（agent 跑的
机器，装 sudogate-client）。通常构建机就是 ssh 客户端，一台机器兼两个
角色；只有走下面的「自建路线」时，ssh 服务器才会临时充当构建机。

依赖：构建需要 Go ≥ 1.24、make、openssl；ssh 服务器走拷贝路线则零依赖；
两端都要传统 sudo（sudo-rs 不支持 askpass）。下文命令里的 `<ssh-server>`
指你的 ssh 服务器（`user@host` 或 ssh config 里的别名）；socket 路径里的
`/run/user/1000/` 是 uid 1000 的写法，哪端 uid 不是 1000 就照改哪端。

一次性生成密钥对（任意第三方工具），带外保管——仓库永不存密钥，每台
参与构建的机器使用同一对：

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/laptop.key
openssl pkey -in keys/laptop.key -pubout -out keys/laptop.pub
```

**ssh 客户端**（你面前的机器）：

```bash
make install-server    # 构建 + ~/.local/bin + systemd --user 服务
make install-plugin    # omarchy 审阅面板（可选，推荐）
```

随后对每台 ssh 服务器建立**专用转发通道**（每台一次；目标需支持免密
BatchMode 登录）：

```bash
make install-forward HOST=<ssh-server>   # 等价 sudogate-server forward add
```

远端 socket 默认 `/run/user/1000/sudogate.sock`（uid-1000 拼写）；目标机
uid 非 1000 时用路径覆盖语法，逐台指定：

```bash
make install-forward HOST=nuc:/run/user/1001/sudogate.sock
```

转发由 **server 内嵌管理**（`~/.config/sudogate/forward.conf`，一行一
主机）：每台目标是一个被看护的 `ssh -N -R` 子进程——不依赖你是否开着
交互会话，故障按退避曲线自动重启，server 退出时统一收割，重连时远端
sshd 会自愈残留的死 socket。随时查看健康状态：

```bash
sudogate-server forward list     # 每台主机 ✓/✗、重启次数、最近错误
```

**不要**在 `~/.ssh/config` 里给目标主机配 `RemoteForward`。任何命中该
块的短命 ssh（workspace 工具的健康探测、一次性 `ssh host '命令'`、
自动重连的挂载）都会抢走 socket 路径、几秒后死去、留下一个远程
`sudo -A` 眼中 `connection refused` 的尸体文件——即便旁边有一个完全
健康的无名转发监听者存在。

**ssh 服务器**（agent 跑的那些机器）——装 client 两条路线：

```bash
# 拷贝路线（推荐）：make install-server 时 client 已一并构建，在 ssh
# 客户端上执行即可（Go 静态二进制，ssh 服务器零依赖）：
ssh <ssh-server> 'mkdir -p ~/.local/bin'
scp build/sudogate-client <ssh-server>:.local/bin/
```

```bash
# 自建路线：ssh 服务器 clone 仓库并放入同一对密钥后 make（注意 inject
# 需要私钥在场——无必要时别让私钥多跑一台机器）
make install-client
```

无论哪条路线，在 ssh 服务器上收尾：

```bash
# 交互 shell：
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
# agent（非交互）环境继承需另配 —— 见「与 agent 协作」
```

**ssh 服务器的 sshd 一次性配置（root）**：sshd 默认在会话断开后不清理
socket 文件（正常退出也留），残留文件会堵死转发重连。让新连接绑定前
自动清掉旧文件（server 内嵌转发的重连自愈依赖它更稳）：

```bash
echo 'StreamLocalBindUnlink yes' | sudo tee /etc/ssh/sshd_config.d/99-sudogate.conf
sudo systemctl restart ssh    # Debian/Ubuntu；其它发行版服务名可能是 sshd
```

注意：该配置下 sshd 绑定前会**无条件清掉旧 socket**——这正是为什么
`~/.ssh/config` 里绝不能给目标主机配 `RemoteForward`（见上文警告）：
短命连接会在同一机制下反复抢走 server 转发监听的文件名。

之后自检再实战：

```bash
# ssh 服务器上（server 的转发通道已在跑，与你的会话无关）：
sudogate-client test   # 端到端自检：ssh 客户端弹出
                       # 「sudogate-client self-test」的审阅请求即链路通
sudo -A <命令>         # 实战
```

**审阅界面** —— 两种方式，同一个 server：

- **omarchy 面板**（`make install-plugin` 安装）：状态栏钥匙徽标（🔑 + 待批数）
  ——有待批时红色醒目，空闲时灰色暗淡（钥匙 + 0）。**默认徽标常驻栏位**，
  一眼可见面板存活；配置 `hideWhenEmpty` 可改为空闲时完全隐藏。点击展开
  审阅卡——主机/用户/目录/完整命令/倒计时，密码框（回车 = 批准）与拒绝
  按钮。到期请求自动滚动并清空密码框。随时
  `git pull && make install-plugin` 升级（幂等安装）。
- **终端 TUI**（`make build` 产出 `build/sudogate-tui`）：纯键盘审批面板——
  fsnotify 监听 server 状态文件（零轮询），请求到来即时出现；`j/k` 上下
  选择、`r` 拒绝、`回车` 弹密码框（隐藏回显，再按回车批准 / Esc 取消）、
  `q` 退出；每条显示剩余秒数倒计时；另附**每主机转发健康灯**
  （✓/✗ 及最近错误，通道状态翻转实时推送）。动作经 ctl socket 与
  面板/CLI 同源；server 未运行只显示提示，恢复后自动跟上。
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

## 端到端示例（laptop → minipc）

具体机器名登场：`laptop` 兼任构建机与 ssh 客户端，`minipc` 是 ssh 服务器
（IP 192.168.1.50，uid 1000）。从零到弹窗走一遍：

```bash
# laptop：生成密钥对、make install-server（见上文），随后把 client 送过去
ssh minipc 'mkdir -p ~/.local/bin'
scp build/sudogate-client minipc:.local/bin/
```

```bash
# minipc：导出 askpass（交互 shell）
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
```

```bash
# minipc（root，一次性）：允许新连接清掉残留 socket
echo 'StreamLocalBindUnlink yes' | sudo tee /etc/ssh/sshd_config.d/99-sudogate.conf
sudo systemctl restart ssh
```

```bash
# laptop：给 server 添加转发目标（热生效；免密需先就绪）
make install-forward HOST=minipc
```

连上后，在 minipc 的会话里：

```bash
sudogate-client test    # 自检：laptop 弹出「sudogate-client self-test」
                        # 审阅请求，说明桥已通
sudo -A whoami          # 实战：读命令 → 输密码 → root
```

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
重试轰炸。agent 被中断（Ctrl+C）时未决审阅立即消失；相同的并发命令各自
独立审阅，逐条批准。

## 当 ssh 由 herdr 这类工具代发时

herdr、VS Code Remote 这类工具连远程时往往自己起 ssh、用自己生成的
配置，你的 `~/.ssh/config` 它们不读。**在内嵌转发管理的架构下这已经
不重要**：转发是 sudogate-server 自己的专用 `ssh -N -R` 子进程，与任何
工具的连接（以及你有没有开交互会话）完全解耦。工具的 ssh 怎么连、
断不断，桥都在。

历史上（v1 架构）转发靠 `~/.ssh/config` 的 `RemoteForward` 搭在"你已
开的 ssh 会话"上，于是有过一整套让 herdr 读用户 config、别名与 IP
同块匹配的配法——**那套配法现在是事故根源，请全部废弃**：herdr 对
机器做健康探测时每几秒拉起一条短命 ssh，只要命中任何带
`RemoteForward` 的 Host 块，就会在 `StreamLocalBindUnlink yes` 的 sshd
上反复抢走转发 socket 的文件名、随后死去留下尸体文件，远程 `sudo -A`
表现为持续的 `connection refused`，而 server 的转发监听被挤成"无名"
状态（`forward list` 看着还是 ✓）。迁移到内嵌转发后，请把
`~/.ssh/config` 里所有 sudogate 相关的 `RemoteForward` 行删掉。

## 桌面弹窗审批（可选）

server 内置桌面弹窗：请求到达且队列非空且弹窗未开时，自动弹出一个
居中浮动的终端窗口跑 `sudogate-tui`，队列清空（批完/拒完/超时/申请人
撤回）后 TUI 自动退出、窗口随之消失。弹窗不会被 server 的停止/重启
信号强杀（独立会话 + service 单元 KillMode=process），窗口总是经 TUI
自身优雅关闭；q 关窗 = 弃管本批（交给超时兜底），之后**新请求到达**
会重新弹（其他端处理条目不会触发重弹）。

### 开启与关闭

```bash
sudogate-server popup on      # 生成默认 kitty 模板并启用
sudogate-server popup status  # 查看状态/模板/弹窗
sudogate-server popup off     # 关闭
```

### 配置即终端

开关只是一个文件：`~/.config/sudogate/popup.conf`（存在即启用，每次
触发时重读，**编辑即时生效**）。内容是弹窗命令模板，`{tui}` 占位符
（必须独立成词）被替换为 `sudogate-tui --until-empty`：

```bash
kitty --app-id sudogate-approve --override initial_window_width=90c --override initial_window_height=26c --override remember_window_size=no -e {tui}
```

换终端改一行即可（更多模板见 `packaging/popup.conf.example`）：
foot 用 `foot --app-id sudogate-approve -e {tui}`、alacritty 用
`alacritty --class sudogate-approve -e {tui}`……server 对终端零知识。

### 窗口浮动规则（Hyprland）

新窗口默认平铺；要"弹框"观感需按窗口标识配一条浮动居中规则。
omarchy 的 lua 配置（`~/.config/hypr/`）写法：

```lua
o.window({ class = "^sudogate-approve$" }, { float = true, size = "800 480", center = true })
```

其他 WM/DE 用各自的窗口规则语法按同样的 class/app-id 匹配即可；
不配规则也能用，只是窗口平铺进布局而非弹框。

### 注意

- server 以 systemd user service 运行时会继承 `WAYLAND_DISPLAY` 等
  会话环境；在无图形环境的机器上开启无效（spawn 失败仅记日志）
- 模板按空白切分、不支持引号——各终端命令行参数都不需要
- 多显示器下窗口出现在焦点所在的显示器（新窗口跟随当前工作区）

## 架构

```
ssh 服务器（agent）                      ssh 客户端（操作者）
───────────────                      ──────────────────────
sudo -A <cmd>
 └─ sudogate-client（= SUDO_ASKPASS）
     · ps 取证真实命令
     · 一次性 X25519 密钥对
           │ ── ssh -R unix socket ──▶ sudogate-server（systemd --user）
           │    （server 内嵌管理的专用通道，              │ 审阅 UI：omarchy 面板
           │      与工具/交互会话无关）                     │ / TUI / CLI
           │    请求 {命令, id, 公钥}                      │ 操作者输入密码
           ◀─ 密封密码 + 签名 ────────────────────────────┘
 └─ 验签（编译内嵌公钥）
     一次性密钥解封 → stdout → sudo
```

- **组件** —— `sudogate-server`（ssh 客户端）：监听由各 ssh 服务器转发来
  的 unix socket，排队请求、密封签名响应；**内嵌转发管理**（每台目标一个
  `ssh -N -R` 子进程，退避重启、随 server 同生共死，`forward.conf` 持久
  化）；以 systemd --user 服务常驻；内嵌私钥。`sudogate-client`（ssh
  服务器）：askpass helper；内嵌公钥、别无秘密，由 sudo 按需调起、跑完
  即退，可复制到任意多台主机。`sudogate-tui`（ssh 客户端）：终端审批
  面板（`make build` 产出），fsnotify 监听状态文件，含每主机转发健康
  灯。`plugin/`（ssh 客户端）：omarchy quickshell 审阅面板——inotify
  监听 server 状态文件（零轮询），经 server 控制子命令批准/拒绝，密码
  走 stdin。
- **信任与密码学** —— 传输层认证/加密继承自 ssh；响应来源由 Ed25519 对
  `id ‖ SHA256(命令) ‖ SHA256(公钥)` 的签名证明，验签用编译内嵌公钥；密码以
  XChaCha20-Poly1305 在 X25519/HKDF 密钥下密封，只有等待中的 askpass 进程
  能打开。
- **已知边界** —— ssh 服务器上同 uid 代码可自行提交请求（钓鱼级；防线是人读
  命令，与本地桌面弹框同级）；同 uid DoS 不可避免；ssh 客户端失陷即
  全盘失陷（私钥在此）。

## 行为（实测）

| 你的操作     | 弹框            | 结果                     |
|--------------|-----------------|--------------------------|
| 输对密码     | 1 次            | 以 root 执行             |
| 输错密码     | 至多 3 次       | sudo 放弃                |
| 直接拒绝     | 仅 1 次         | 立即终止                 |
| 空输入       | 仅 1 次         | 视为拒绝                 |

并发请求各自独立密封与决断；相同的并发命令各自独立审阅（逐条人工批准
的节奏天然串行化 apt/dpkg 这类锁竞争的执行）。client 断连（Ctrl+C）
立即撤销自己那条审阅。

## 排障

- **`sudo：没有指定 askpass 程序`** —— `SUDO_ASKPASS` 没到 sudo 进程。agent
  的 shell 非交互、不读 rc 文件；环境链条见[「与 agent 协作」](#与-agent-协作)。
- **`remote port forwarding failed for listen path …`** —— 出现在
  `sudogate-server forward list` 的 ✗ 详情里。多为 ssh 服务器的 sshd
  默认 `StreamLocalBindUnlink no`：连接断开后 socket 文件残留（正常退出
  也留），堵死转发重连。立即清：

  ```bash
  ssh -o ClearAllForwardings=yes <ssh-server> 'rm -f /run/user/1000/sudogate.sock'
  ```

  一次性根治见[快速开始](#快速开始)的 `StreamLocalBindUnlink yes`。

- **`sudo -A` 持续 `connection refused` 但 `forward list` 全 ✓** —— 十有
  八九是 `~/.ssh/config` 里残留了 sudogate 的 `RemoteForward`：某些工具
  （herdr 健康探测、自动重连的挂载）的短命 ssh 命中该块，在
  `StreamLocalBindUnlink yes` 的 sshd 上反复抢走转发 socket 的文件名
  后死去，留下一具远端 connect 时的"尸体"。删掉 config 里的
  `RemoteForward` 行，由 server 内嵌转发独占（详见[「当 ssh 由 herdr
  这类工具代发时」](#当-ssh-由-herdr-这类工具代发时)）。

- **人不在时 `sudo -A` 失败** —— server 挂了才会如此（内嵌转发随
  server 常驻，与你是否在线无关）；`systemctl --user status
  sudogate` 排查。
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
