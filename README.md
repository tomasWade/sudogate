# SudoGate

**Reverse askpass** — sudo password prompts from remote AI agents, delivered to the machine you're sitting at.

[English](README.md) · [简体中文](README.zh-CN.md)

## Why

Running a coding agent (opencode, Claude Code, codex, ...) on a remote host means it
will eventually need `sudo`. The usual options are all bad:

- **`NOPASSWD` sudoers** — every piece of code the agent runs now holds root
- **Pasting the password into chat** — it lands in the model context and transcripts
- **Forwarding a GUI dialog** — needs a forwarded display, useless on headless boxes

SudoGate takes another route: the agent runs plain `sudo -A`, and the password
prompt travels back **through the ssh session you already have open** to your ssh
client machine. Every elevation is read and approved by a human; the password is
typed fresh each time and never stored on either machine.

## Features

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

## Quick Start

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
# interactive shells:
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
# agent (non-interactive) inheritance needs more — see "Working with agents"
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

## Working with agents

SudoGate is the **second** of two gates. The first lives in your agent's own
permission config; together they give every elevation two independent human
approvals.

**Gate 1 — the agent asks before running sudo.** For opencode:

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

Claude Code and codex have equivalents; anything that can require human
confirmation for `sudo` commands works as Gate 1.

**Gate 2 — SudoGate asks before the password travels.** The full flow:

```
agent wants root
  → Gate 1: agent tool asks "run sudo -A …?" — you approve the intent
  → agent runs sudo -A <cmd>
  → sudogate-client forwards the request over your ssh session
  → Gate 2: the key badge on your bar turns red; open the panel and
    read host / user / cwd / full command
  → command matches what Gate 1 just showed? type the password (Enter).
    Anything else — deny.
  → sudo runs (or aborts); the agent sees only the exit code
```

The review discipline that makes phishing hard: **approve only requests whose
command matches what Gate 1 just approved**. A command you never sanctioned,
a host you have no session into — deny.

**Wiring the agent's environment.** `sudo` reads `SUDO_ASKPASS` from its own
environment, inherited from the agent process. Agent shells are
non-interactive: they read **no rc files**, so the `~/.profile` export from
Quick Start only covers interactive sessions. The variable has to be in the
agent process environment:

- Agent started from an interactive shell — it inherits that shell's
  environment. Put the export **before** the interactivity guard in
  `~/.bashrc` (or in `~/.zshenv` for zsh) so every child gets it:

  ```bash
  # first line of ~/.bashrc — BEFORE the "case $- in *i*)" guard
  export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client
  ```

- Agent started by systemd (or any supervisor) — set it in the unit:

  ```ini
  [Service]
  Environment=SUDO_ASKPASS=%h/.local/bin/sudogate-client
  ```

Verify from the agent's own shell tool:

```bash
echo "${SUDO_ASKPASS:-MISSING}"
```

**What the agent experiences** — `sudo -A` blocks until a decision:

| Operator action      | Agent sees                            |
|----------------------|---------------------------------------|
| Password entered     | command runs as root, normal exit     |
| Deny                 | exit 1, askpass error on stderr       |
| Timeout (default 120s) | exit 1, timeout error on stderr     |

The wait is a human reading the command — agents should treat blocking as
normal and not retry-loop a pending request (identical concurrent commands
are merged into one review anyway).

## Architecture

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

## Behavior (measured)

| Your action          | Dialogs           | Result                          |
|----------------------|-------------------|---------------------------------|
| Correct password     | 1                 | command runs as root            |
| Wrong password       | up to 3 (retries) | sudo gives up                   |
| Deny                 | 1 only            | immediate abort                 |
| Empty input          | 1 only            | treated as deny                 |

Concurrent requests are sealed and decided independently; identical concurrent
commands are merged into one review.

## Troubleshooting

- **`sudo: no askpass program specified`** — `SUDO_ASKPASS` never reached the
  sudo process. Agent shells are non-interactive and read no rc files; see
  [Working with agents](#working-with-agents) for the environment chain.
- **`remote port forwarding failed for listen path …`** — a stale
  `/run/user/1000/sudogate.sock` (left behind by a dropped session) blocks the
  forward, and `ExitOnForwardFailure` then drops the connection. Clear it with
  a forwarding-free one-liner:

  ```bash
  ssh -o ClearAllForwardings=yes minipc 'rm -f /run/user/1000/sudogate.sock'
  ```

- **`sudo -A` fails while you're away** — that is the design
  (disconnected = denied); the bridge only exists inside your live ssh session.
- **sudo-rs** — does not support askpass at all; use traditional sudo
  (`sudo` from sudo.ws).
- **`sudo -n true` still asks for a password** — correct: SudoGate deliberately
  adds no password-less path. There is nothing to configure in sudoers.

## References

- [lllamnyp/askpass](https://github.com/lllamnyp/askpass) — same pattern over mTLS
- [OzoneAsai/remote-askpass](https://github.com/OzoneAsai/remote-askpass) — Windows agent variant
- [crypdick/sudoplz](https://github.com/crypdick/sudoplz) — askpass with SSH-key-encrypted storage
- [0xMH/sudo-mcp](https://github.com/0xMH/sudo-mcp) — MCP tool popping a local OS dialog (agent and human on the same machine)
- [hughesjs/sudo-mcp](https://github.com/hughesjs/sudo-mcp) — MCP + polkit/pkexec (needs a local desktop session)

## License

[MIT](LICENSE) — Copyright (c) 2026 tomasWade
