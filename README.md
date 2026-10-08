# SudoGate

**Reverse askpass** — sudo password prompts from AI agents on ssh servers, delivered to the machine you're sitting at.

[English](README.md) · [简体中文](README.zh-CN.md)

## Why

Running a coding agent (opencode, Claude Code, codex, ...) on an ssh server means it
will eventually need `sudo`. The usual options are all bad:

- **`NOPASSWD` sudoers** — every piece of code the agent runs now holds root
- **Pasting the password into chat** — it lands in the model context and transcripts
- **Forwarding a GUI dialog** — needs a forwarded display, useless on headless boxes

SudoGate takes another route: the agent runs plain `sudo -A`, and the password
prompt travels back **through the ssh session you already have open** to your ssh
client. Every elevation is read and approved by a human; the password is
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
- Disconnected = denied — with the ssh-client machine (where the server runs) off or unreachable, the forward dies with it and remote `sudo -A` simply fails
- Audit trail — every decision logged as JSONL
- Pairs with your agent's own gate — e.g. opencode's `"*sudo*": "ask"` rule,
  giving two independent approvals per command

## Quick Start

Three roles first: the **build machine** (has the repo and keys, runs `make`),
the **ssh client** (your machine, runs sudogate-server and the review UI), and
the **ssh server** (the machine agents run on, gets sudogate-client). Usually
the build machine and the ssh client are the same box; only the "build on the
server" route below turns the ssh server into a build machine.

Requirements: building needs Go ≥ 1.24, make, openssl; the ssh server needs
nothing beyond traditional sudo if you copy the binary over (sudo-rs has no
askpass support). In the commands below, `<ssh-server>` means your ssh server
(`user@host` or its alias in ssh config); the `/run/user/1000/` in socket paths
is the uid-1000 spelling — change it on whichever end isn't uid 1000.

Generate a keypair once, with any third-party tool, and keep it out of band — the
repo never stores keys, and every machine that builds needs the same pair:

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/laptop.key
openssl pkey -in keys/laptop.key -pubout -out keys/laptop.pub
```

On the **ssh client** (the machine in front of you):

```bash
make install-server    # build + ~/.local/bin + systemd --user service
make install-plugin    # omarchy review panel (optional, recommended)
```

Then establish the dedicated forward channel to each ssh server (once per
machine; the target must accept passwordless BatchMode ssh):

```bash
make install-forward HOST=<ssh-server>   # same as sudogate-server forward add
```

The remote socket defaults to `/run/user/1000/sudogate.sock` (uid-1000
spelling); override it per host when the target's uid differs:

```bash
make install-forward HOST=nuc:/run/user/1001/sudogate.sock
```

Forwards are managed **inside the server** (`~/.config/sudogate/forward.conf`,
one host per line): each target runs as a supervised `ssh -N -R` child
process — alive whether or not you have an interactive session, restarted
with backoff on failure, reaped when the server exits, and the remote sshd
self-heals stale sockets on reconnect. Inspect at any time:

```bash
sudogate-server forward list     # per-host ✓/✗, restart count, last error
```

**Do not** put `RemoteForward` in `~/.ssh/config` for the target host. Any
short-lived ssh that matches such a block (health probes from workspace
tools, one-off `ssh host 'cmd'` runs, reconnecting mounts) will steal the
socket path, die seconds later, and leave a corpse file that remote `sudo -A`
reads as `connection refused` — even while a perfectly healthy forward
listener exists namelessly beside it.

On each **ssh server** (the machines agents run on) — two ways to install the client:

```bash
# Copy route (recommended): the client was already built by make install-server;
# run this on the ssh client (static Go binary, zero deps on the server):
ssh <ssh-server> 'mkdir -p ~/.local/bin'
scp build/sudogate-client <ssh-server>:.local/bin/
```

```bash
# Build-on-the-server route: clone the repo on the ssh server, place the same
# keypair in keys/, then make (note that inject needs the private key present —
# don't take the private key to more machines than necessary)
make install-client
```

Either way, finish on the ssh server:

```bash
# interactive shells:
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
# agent (non-interactive) inheritance needs more — see "Working with agents"
```

**One-time sshd config on the ssh server (root)**: sshd does not remove the
socket file when a connection ends (even a clean exit), and the stale file
blocks forward re-establishment. Make each new connection unlink it before
binding (the server's embedded forward relies on this for reliable self-heal):

```bash
echo 'StreamLocalBindUnlink yes' | sudo tee /etc/ssh/sshd_config.d/99-sudogate.conf
sudo systemctl restart ssh    # Debian/Ubuntu; other distros may call it sshd
```

Note that with this enabled, sshd unlinks the old socket **unconditionally**
before binding — which is exactly why `RemoteForward` must never sit in
`~/.ssh/config` for the target host (see the warning above): short-lived
connections would repeatedly steal the socket filename from the server's
forward listener under the very same mechanism.

Then self-test and use it for real:

```bash
# on the ssh server (the server's forward channel is already running,
# regardless of your interactive sessions):
sudogate-client test   # end-to-end self test: a review request reading
                       # “sudogate-client self-test” pops up on the ssh client
sudo -A <command>      # the real thing
```

**Review UI** — three ways, same server:

- **omarchy panel** (what `make install-plugin` installs): a key badge
  (🔑 + pending count) on the bar — urgent-red while requests wait, dimmed
  gray (key + 0) when idle. By default the badge always occupies its slot, so
  a glance tells you the panel is alive; set `hideWhenEmpty` to collapse it
  while idle. Click to open the review card — host / user / cwd / full
  command / countdown, password field (Enter = approve), deny button.
  Deadline expiry rolls the queue and clears the password field automatically.
  Upgrade anytime with `git pull && make install-plugin` (idempotent).
- **Terminal TUI** (`make build` produces `build/sudogate-tui`): a
  keyboard-only review panel — fsnotify watches the server state file
  (zero polling), so new requests appear instantly; `j/k` to move, `r` to
  deny, `Enter` opens a masked password prompt (Enter again = approve,
  Esc = cancel), `q` quits; each row shows a per-request countdown, plus
  **per-host forward health lights** (✓/✗ with last error, pushed on every
  channel state change). Actions go through the same ctl socket as the panel
  and CLI; if the server is down the TUI just says so and picks up
  automatically once it is back — there is no connection to reconnect.
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

## Worked example (laptop → minipc)

Real names now: `laptop` doubles as build machine and ssh client; `minipc` is
the ssh server (IP 192.168.1.50, uid 1000). From zero to the first prompt:

```bash
# laptop: generate the keypair, make install-server (see above), then ship the client
ssh minipc 'mkdir -p ~/.local/bin'
scp build/sudogate-client minipc:.local/bin/
```

```bash
# minipc: export the askpass (interactive shell)
echo 'export SUDO_ASKPASS=$HOME/.local/bin/sudogate-client' >> ~/.profile
```

```bash
# minipc (root, once): let new sessions clear a stale socket
echo 'StreamLocalBindUnlink yes' | sudo tee /etc/ssh/sshd_config.d/99-sudogate.conf
sudo systemctl restart ssh
```

```bash
# laptop: add the forward target to the server (hot; passwordless ssh
# must already work)
make install-forward HOST=minipc
```

Once connected, inside the session on minipc:

```bash
sudogate-client test    # self test: a “sudogate-client self-test” review
                        # pops up on laptop — the bridge works
sudo -A whoami          # for real: read the command → type password → root
```

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

## When a tool such as herdr runs the ssh for you

Tools like herdr or VS Code Remote usually spawn their own ssh with a config
they generate — your `~/.ssh/config` is never read. **With the embedded
forward manager this no longer matters**: the forward is the server's own
dedicated `ssh -N -R` child process, fully decoupled from any tool's
connections and from whether you have an interactive session open. The bridge
is up regardless of how the tool connects or disconnects.

Historically (v1 design) the forward rode "the ssh session you already had
open" via `RemoteForward` in `~/.ssh/config`, and there was a whole recipe for
making herdr read the user config with alias-plus-IP Host blocks — **that
recipe is now a hazard, discard it**: herdr's health probes spawn a
short-lived ssh every few seconds, and any `RemoteForward` block they match
will, on a `StreamLocalBindUnlink yes` sshd, repeatedly steal the forward
socket's filename and then die leaving a corpse file. Remote `sudo -A` shows
a persistent `connection refused` while the server's listener — now
nameless — still looks ✓ in `forward list`. After migrating to the embedded
forward, delete every sudogate-related `RemoteForward` line from
`~/.ssh/config`.

## Architecture

```
ssh server (agent)                     ssh client (operator)
───────────────────                    ─────────────────────
sudo -A <cmd>
 └─ sudogate-client (= SUDO_ASKPASS)
     · captures real command via ps(1)
     · one-time X25519 keypair
          │ ── ssh -R unix socket ──▶ sudogate-server (systemd --user)
          │   (dedicated channel managed by         │ review UI: omarchy panel
          │    the server itself; independent       │ / TUI / CLI
          │    of tools & interactive sessions)     │ operator types password
          │    request {cmd, id, pubkey}            │
          ◀─ sealed password + signature ───────────┘
 └─ verify signature (compiled-in pubkey)
     unseal with the one-time key → stdout → sudo
```

- **Components** — `sudogate-server` (ssh client): listens on a unix socket
  forwarded from each ssh server, queues requests, seals and signs responses;
  **embedded forward management** (one supervised `ssh -N -R` child per
  target, backoff restarts, lifetime bound to the server, persisted in
  `forward.conf`); runs as a systemd --user service; embeds the private key.
  `sudogate-client` (ssh server): the askpass helper; embeds the public key
  and nothing else, runs on demand per sudo invocation, copy it to any number
  of hosts. `sudogate-tui` (ssh client): terminal review panel (built by
  `make build`), watches the state file with fsnotify, includes per-host
  forward health lights. `plugin/` (ssh client): the omarchy quickshell
  review panel — watches the server's state file with inotify (no polling),
  approves/denies via the server's control subcommands, password travels via
  stdin.
- **Trust & crypto** — transport auth/encryption inherited from ssh; response
  origin proven by an Ed25519 signature over `id ‖ SHA256(cmd) ‖ SHA256(pubkey)`
  against the compiled-in public key; password sealed with XChaCha20-Poly1305
  under an X25519/HKDF key so that only the waiting askpass process can open it.
- **Known limits** — same-uid code on the ssh server can submit its own request
  (phishing-class; the defense is reading the command, same as with local desktop
  prompts). Same-uid DoS is unavoidable. A compromised ssh client is game
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
- **`remote port forwarding failed for listen path …`** — shows up in the ✗
  details of `sudogate-server forward list`. Usually the ssh server's sshd
  defaults to `StreamLocalBindUnlink no`: the socket file survives connection
  teardown (**clean exits too**) and blocks forward re-establishment. Clear it
  now:

  ```bash
  ssh -o ClearAllForwardings=yes <ssh-server> 'rm -f /run/user/1000/sudogate.sock'
  ```

  For a permanent fix, see `StreamLocalBindUnlink yes` in
  [Quick Start](#quick-start).

- **`sudo -A` persistently `connection refused` while `forward list` is all ✓** —
  almost certainly a leftover sudogate `RemoteForward` in `~/.ssh/config`:
  short-lived connections from some tool (herdr health probes, auto-reconnecting
  mounts) match that block and, on a `StreamLocalBindUnlink yes` sshd,
  repeatedly steal the forward socket's filename and die, leaving a corpse
  that remote connects read as refused. Delete the `RemoteForward` lines and
  let the embedded forward own the socket (see
  [When a tool such as herdr runs the ssh for you](#when-a-tool-such-as-herdr-runs-the-ssh-for-you)).

- **`sudo -A` fails while you're away** — only if the server itself is down
  (the embedded forward lives with the server, not with your sessions);
  check `systemctl --user status sudogate`.
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
