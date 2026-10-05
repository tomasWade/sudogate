# SudoGate

**Reverse askpass** — sudo password prompts from remote AI agents, delivered to the machine you're sitting at.
**反向 askpass** — 远程主机上 AI agent 的 sudo 密码框，弹回你面前这台机器。

[English](#english) · [中文](#中文)

---

## English

### Why

Running a coding agent (opencode, Claude Code, codex, ...) on a remote host means it
will eventually need `sudo`. The usual options are all bad:

- **`NOPASSWD` sudoers** — every piece of code the agent runs now holds root
- **Pasting the password into chat** — it lands in the model context and transcripts
- **Forwarding a GUI dialog** — needs a forwarded display, useless on headless boxes

SudoGate takes another route: the agent runs plain `sudo -A`, and the password
prompt travels back **through the ssh session you already have open** to your ssh
client machine. Every elevation is read and approved by a human; the password is
typed fresh each time and never stored on either machine.

### Features

- Human in the loop, per command — one review per elevation; deny stops it instantly
- Zero password persistence — nothing at rest on either end
- No new trust surface — transport is an ssh `-R` unix-socket forward; authentication
  and encryption come from ssh itself. No ports, no certificates
- Compile-time key split — server binary embeds the private key, client binary embeds
  the public key; the codebase has no key-generation path
- Tamper-proof by construction — per-request X25519 sealing, Ed25519 signatures,
  one-time ids and command-hash binding
- Disconnected = denied — if you're not on ssh, remote `sudo -A` simply fails
- Audit trail — every decision logged as JSONL
- Pairs with your agent's own gate — e.g. opencode's `"*sudo*": "ask"` rule,
  giving two independent approvals per command

### Quick Start

Requirements: Go ≥ 1.24, make, openssl; traditional sudo (sudo-rs has no askpass support).

Generate a keypair once, with any third-party tool, and keep it out of band — the
repo never stores keys, and every machine that builds needs the same pair:

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/laptop.key
openssl pkey -in keys/laptop.key -pubout -out keys/laptop.pub
```

On the **ssh client machine** (the machine you ssh from):

```bash
make install-server    # build + ~/.local/bin + systemd --user service
make install-plugin    # omarchy review panel (optional, recommended)

# ~/.ssh/config:
Host minipc
    RemoteForward /run/user/1000/sudogate.sock /run/user/1000/sudogate.sock
    ExitOnForwardFailure yes
```

On each **remote host** (the machines agents run on):

```bash
make install-client    # askpass binary to ~/.local/bin
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
sudogate-client test   # prints the embedded pubkey fingerprint
```

Then from an agent or a shell: `sudo -A <command>` — the request appears on your
ssh client machine for review.

**Review UI** — two ways, same server:

- **omarchy panel** (what `make install-plugin` installs): a key badge
  (🔑 + pending count) on the bar — urgent-red while requests wait, dimmed
  gray (key + 0) when idle. By default the badge always occupies its slot, so
  a glance tells you the panel is alive; set `hideWhenEmpty` to collapse it
  while idle. Click to open the review card — host / user / cwd / full
  command / countdown, password field (Enter = approve), deny button.
  Deadline expiry rolls the queue and clears the password field automatically.
  Upgrade anytime with `git pull && make install-plugin` (idempotent).
- **CLI fallback** (any environment):

  ```bash
  sudogate-server status   # list pending requests
  sudogate-server review   # review the oldest: shows the command, hidden password input;
                           #   password + Enter = approve, empty Enter = deny
  ```

**Panel options** — set inline on the layout entry in
`~/.config/omarchy/shell.json`; changes apply immediately (no shell restart):

```jsonc
{ "id": "tomaswade.sudogate", "hideWhenEmpty": true, "timeoutSec": 120 }
```

| Option | Default | Meaning |
|---|---|---|
| `hideWhenEmpty` | `false` | `false`: badge always visible (dimmed key + 0 when idle); `true`: badge hidden while the queue is empty |
| `timeoutSec` | `120` | request deadline driving the countdown; keep in sync with the server's `-timeout` |
| `runtimeDir` | `""` | directory holding `sudogate.sock.ctl` / `sudogate.state`; empty = `XDG_RUNTIME_DIR` |
| `serverBin` | `""` | `sudogate-server` path used for approve/deny; empty = auto-detect (`~/.local/bin` first) |
| `demo` | `false` | render canned requests (no server needed) for previewing the visuals |

### Architecture

```
remote host (agent)                    ssh client machine (operator)
───────────────────                   ─────────────────────────────
sudo -A <cmd>
 └─ sudogate-client (= SUDO_ASKPASS)
     · captures real command via ps(1)
     · one-time X25519 keypair
          │ ── ssh -R unix socket ──▶ sudogate-server (systemd --user)
          │    request {cmd, id, pubkey}     │ review UI: omarchy panel or CLI
          │                                  │ operator types password
          ◀─ sealed password + signature ────┘
 └─ verify signature (compiled-in pubkey)
     unseal with the one-time key → stdout → sudo
```

- **Components** — `sudogate-server` (ssh client machine): listens on a unix socket
  forwarded from each remote host, queues requests, seals and signs responses;
  runs as a systemd --user service; embeds the private key. `sudogate-client`
  (remote host): the askpass helper; embeds the public key and nothing else, runs
  on demand per sudo invocation, copy it to any number of hosts. `plugin/` (ssh
  client machine): the omarchy quickshell review panel — watches the server's
  state file with inotify (no polling), approves/denies via the server's control
  subcommands, password travels via stdin.
- **Trust & crypto** — transport auth/encryption inherited from ssh; response
  origin proven by an Ed25519 signature over `id ‖ SHA256(cmd) ‖ SHA256(pubkey)`
  against the compiled-in public key; password sealed with XChaCha20-Poly1305
  under an X25519/HKDF key so that only the waiting askpass process can open it.
- **Known limits** — same-uid code on the remote host can submit its own request
  (phishing-class; the defense is reading the command, same as with local desktop
  prompts). Same-uid DoS is unavoidable. A compromised ssh client machine is game
  over (it holds the private key).

### Behavior (measured)

| Your action          | Dialogs           | Result                          |
|----------------------|-------------------|---------------------------------|
| Correct password     | 1                 | command runs as root            |
| Wrong password       | up to 3 (retries) | sudo gives up                   |
| Deny                 | 1 only            | immediate abort                 |
| Empty input          | 1 only            | treated as deny                 |

Concurrent requests are sealed and decided independently; identical concurrent
commands are merged into one review.

### References

- [lllamnyp/askpass](https://github.com/lllamnyp/askpass) — same pattern over mTLS
- [OzoneAsai/remote-askpass](https://github.com/OzoneAsai/remote-askpass) — Windows agent variant
- [crypdick/sudoplz](https://github.com/crypdick/sudoplz) — askpass with SSH-key-encrypted storage
- [0xMH/sudo-mcp](https://github.com/0xMH/sudo-mcp) — MCP tool popping a local OS dialog (agent and human on the same machine)
- [hughesjs/sudo-mcp](https://github.com/hughesjs/sudo-mcp) — MCP + polkit/pkexec (needs a local desktop session)

### License

[MIT](LICENSE) — Copyright (c) 2026 tomasWade

---

## 中文

### 为什么

在远程主机上跑 coding agent（opencode / Claude Code / codex……）迟早要 `sudo`。
常见选择都不理想：

- **sudoers 免密** —— agent 跑的一切代码从此握着 root
- **把密码贴进对话** —— 密码进入模型上下文与转录
- **转发 GUI 弹窗** —— 依赖转发显示，headless 主机不可用

SudoGate 换了一条路：agent 照常 `sudo -A`，密码框沿着**你已经开着的 ssh 会话**
弹回你的 ssh 客户端机器。每次提权都由人读命令、人输密码；密码每次现输，
两端零落盘。

### 特性

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

### 快速开始

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
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
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

### 架构

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

### 行为（实测）

| 你的操作     | 弹框            | 结果                     |
|--------------|-----------------|--------------------------|
| 输对密码     | 1 次            | 以 root 执行             |
| 输错密码     | 至多 3 次       | sudo 放弃                |
| 直接拒绝     | 仅 1 次         | 立即终止                 |
| 空输入       | 仅 1 次         | 视为拒绝                 |

并发请求各自独立密封与决断；相同的并发命令自动合并为一次审阅。

### 参考

- [lllamnyp/askpass](https://github.com/lllamnyp/askpass) —— 同模式，mTLS 传输
- [OzoneAsai/remote-askpass](https://github.com/OzoneAsai/remote-askpass) —— Windows 侧变体
- [crypdick/sudoplz](https://github.com/crypdick/sudoplz) —— SSH 密钥加密存储型 askpass
- [0xMH/sudo-mcp](https://github.com/0xMH/sudo-mcp) —— MCP 工具弹本机 OS 密码框（agent 与人须同机）
- [hughesjs/sudo-mcp](https://github.com/hughesjs/sudo-mcp) —— MCP + polkit/pkexec（依赖本机桌面会话）

### 许可证

[MIT](LICENSE) — Copyright (c) 2026 tomasWade
