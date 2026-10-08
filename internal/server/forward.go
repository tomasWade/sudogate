package server

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ForwardState 是某台主机转发通道的健康快照（ctl/TUI 消费）。
type ForwardState struct {
	Host       string `json:"host"`
	RemotePath string `json:"remote_path,omitempty"`
	Running    bool   `json:"running"`
	Restarts   int    `json:"restarts"`
	LastStart  string `json:"last_start,omitempty"`
	LastErr    string `json:"last_err,omitempty"`
}

// forwardDelays 与 TUI 重连同源的退避曲线：快首试 + 指数退避 + 5s 封顶。
var forwardDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second}

func forwardDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= len(forwardDelays) {
		return forwardDelays[len(forwardDelays)-1]
	}
	return forwardDelays[attempts]
}

// 存活超过该时长后退出视为"跑稳过"，退避重置。
const forwardStable = 60 * time.Second

type fwdHost struct {
	host       string
	remotePath string
	stopOnce   sync.Once
	stop       chan struct{}
	stopped    chan struct{}
	mu         sync.Mutex
	state      ForwardState
}

func (h *fwdHost) snapshot() ForwardState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func (h *fwdHost) setState(f func(*ForwardState)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.state)
}

// ForwardManager 维护到各 ssh 服务器的专用转发通道（ssh -N -R 子进程），
// 取代独立的 systemd sudogate-forward@.service 单元。
type ForwardManager struct {
	mu       sync.Mutex
	confPath string
	sockPath string
	// sshCommand 供测试注入 stub；生产路径为 "ssh"。
	sshCommand string
	hosts      map[string]*fwdHost

	cbMu     sync.Mutex
	onChange func() // 状态翻转回调（SetOnChange 注册，Start 前完成）
}

func NewForwardManager(confPath, sockPath string) *ForwardManager {
	return &ForwardManager{
		confPath:   confPath,
		sockPath:   sockPath,
		sshCommand: "ssh",
		hosts:      map[string]*fwdHost{},
	}
}

// DefaultForwardConf 返回默认配置文件路径。
func DefaultForwardConf() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "sudogate", "forward.conf")
}

// Start 读取配置并拉起所有 supervisor。文件缺失视为空配置；
// 非法行（含空白、缺主机名）跳过，不为其建立永败的 supervisor。
func (m *ForwardManager) Start() {
	for _, line := range m.loadConf() {
		host, path, ok := parseForwardLine(line)
		if !ok {
			continue
		}
		m.startHost(host, path)
	}
}

// Shutdown 收割全部子进程（SIGTERM，2s 后 SIGKILL）。
func (m *ForwardManager) Shutdown() {
	m.mu.Lock()
	hosts := make([]*fwdHost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hosts = append(hosts, h)
	}
	m.mu.Unlock()
	for _, h := range hosts {
		m.stopHost(h)
	}
}

// List 返回全部转发通道状态快照。
func (m *ForwardManager) List() []ForwardState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ForwardState, 0, len(m.hosts))
	for _, h := range m.hosts {
		out = append(out, h.snapshot())
	}
	return out
}

// defaultRemoteSock 是远端 socket 的默认路径（uid-1000 拼写）；
// 目标机 uid 非 1000 时用 "host:/path/to/sock" 语法逐台覆盖。
const defaultRemoteSock = "/run/user/1000/sudogate.sock"

// parseForwardLine 解析一条配置/add 目标："host" 或 "host:/abs/path/sock"
// （仅当冒号后以 "/" 开头才视为路径，避免误拆 user@host:22 之类写法）。
func parseForwardLine(line string) (host, remotePath string, ok bool) {
	line = strings.TrimSpace(line)
	// # 开头视为注释（含无空格的手写注释行），不为其建 supervisor。
	if line == "" || strings.HasPrefix(line, "#") || strings.ContainsAny(line, " \t\n") {
		return "", "", false
	}
	if i := strings.Index(line, ":"); i >= 0 && strings.HasPrefix(line[i+1:], "/") {
		host, path := line[:i], line[i+1:]
		if host == "" || path == "" {
			return "", "", false
		}
		return host, path, true
	}
	// 尾冒号无路径（"host:"）是畸形目标，别让它变成永败的 supervisor。
	if strings.HasSuffix(line, ":") {
		return "", "", false
	}
	return line, defaultRemoteSock, true
}

// canonicalLine 返回一条目标的规范化配置行：默认路径只写 host，
// 覆盖路径写 host:/abs/path。Add 与 rewriteConf 必须共用此规则，
// 否则 Add 落盘的行会在重启后丢失路径覆盖。
func canonicalLine(host, remotePath string) string {
	if remotePath != defaultRemoteSock {
		return host + ":" + remotePath
	}
	return host
}

// Add 热添加一台主机并持久化（幂等：已存在为 no-op）。全程持锁且落盘
// 走 rewriteConfLocked（以内存 map 重建整份文件），与 Remove 的重写天然
// 串行化，杜绝"append 的行被并发的整份重写截断丢失"；顺带清理手编
// 配置里的孤儿行与重复行。目标支持 "host" 或 "host:/abs/path"。
func (m *ForwardManager) Add(line string) error {
	host, remotePath, ok := parseForwardLine(line)
	if !ok {
		return fmt.Errorf("invalid target %q", line)
	}
	m.mu.Lock()
	if existing, exists := m.hosts[host]; exists {
		cur := existing.remotePath
		m.mu.Unlock()
		if cur != remotePath {
			// 换路径必须显式 remove 后重加：静默沿用旧路径会让 README
			// 的 uid 覆盖流程"看似成功实则没换"，远端 sudo -A 持续失败。
			return fmt.Errorf("host %s already forwards to %s; run `forward remove %s` first to change the path", host, cur, host)
		}
		return nil
	}
	m.startHostLocked(host, remotePath)
	err := m.rewriteConfLocked()
	m.mu.Unlock()
	// notify 必须在锁外：onChange 的实现（server.notifyState →
	// fwd.List）会重新获取 m.mu，锁内回调即死锁。
	m.notify()
	return err
}

// Remove 热移除一台主机并持久化。与 Add 同样的规范化；无论该主机
// 是否在运行都重建配置文件（内存 map 为准），conf-only 的手编行
// 也会被清掉，不会重启复活。stopHost 在锁内 join：supervise/spawn
// 不回取 m.mu，无死锁风险，代价是 ctl 调用方最多阻塞一个收割周期
// （SIGTERM→2s→SIGKILL，常态毫秒级）。
func (m *ForwardManager) Remove(host string) error {
	host, _, ok := parseForwardLine(host)
	if !ok {
		return fmt.Errorf("invalid target %q", host)
	}
	m.mu.Lock()
	h, exists := m.hosts[host]
	if exists {
		delete(m.hosts, host)
	}
	err := m.rewriteConfLocked()
	m.mu.Unlock()
	if exists {
		// stopHost 必须在锁外 join：supervise 的 notify 回调会经
		// fwd.List() 重新获取 m.mu，锁内等待即死锁。锁外收割的竞态
		// （并发的同 host Add 抢先启动）由远端 sshd bind 竞争自愈。
		m.stopHost(h)
	}
	m.notify() // 锁外，理由同 Add
	return err
}

// startHost 是 startHostLocked 的加锁包装（测试与未来调用方使用）。
func (m *ForwardManager) startHost(host, remotePath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startHostLocked(host, remotePath)
}

// startHostLocked 要求调用方持有 m.mu。
func (m *ForwardManager) startHostLocked(host, remotePath string) {
	if _, ok := m.hosts[host]; ok {
		return
	}
	h := &fwdHost{
		host:       host,
		remotePath: remotePath,
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
		state:      ForwardState{Host: host, RemotePath: remotePath},
	}
	m.hosts[host] = h
	go m.supervise(h)
}

// stopHost 发出停止信号并等待 supervisor 收场。sync.Once 保证并发调用
// （Remove 竞 Shutdown 快照）不会二次 close 通道 panic。
func (m *ForwardManager) stopHost(h *fwdHost) {
	h.stopOnce.Do(func() { close(h.stop) })
	<-h.stopped
}

// supervise 看护单台主机的 ssh 子进程。状态每次真实翻转后经 m.notify
// 回调通知外界（server 据此重写 state 文件，TUI/面板经 fsnotify 感知）。
func (m *ForwardManager) supervise(h *fwdHost) {
	defer close(h.stopped)
	attempts := 0
	for {
		select {
		case <-h.stop:
			return
		default:
		}
		started := time.Now()
		err := m.spawn(h)
		// 优雅停止（Remove/Shutdown）：不计重启、不写 LastErr——该 host
		// 已出列，避免给观察者留下"停止即失败"的假象；但 Running 要清，
		// 子进程确实没了，Shutdown 后的快照不应停留在"运行中"。
		select {
		case <-h.stop:
			h.setState(func(s *ForwardState) { s.Running = false })
			return
		default:
		}
		if time.Since(started) >= forwardStable {
			attempts = 0 // 跑稳过再死：退避重新从快首试开始
		}
		h.setState(func(s *ForwardState) {
			s.Running = false
			s.Restarts++
			if err != nil {
				s.LastErr = err.Error()
			}
		})
		m.notify()
		select {
		case <-h.stop:
			return
		case <-time.After(forwardDelay(attempts)):
		}
		attempts++
	}
}

// notify 安全地触发一次 onChange 回调。回调在 supervisor goroutine 上
// 执行，server 注入的实现自行加锁；nil 回调为 no-op（测试/独立使用）。
func (m *ForwardManager) notify() {
	if f := m.getOnChange(); f != nil {
		f()
	}
}

func (m *ForwardManager) getOnChange() func() {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	return m.onChange
}

// SetOnChange 注册状态变化回调（须在 Start 之前调用，避免并发注册）。
func (m *ForwardManager) SetOnChange(f func()) {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	m.onChange = f
}

// spawn 拉起一次 ssh -N -R 并阻塞至其退出。命令行与原 systemd 单元等价；
// ExitOnForwardFailure=yes 使 bind 失败立即退出交给退避重试，远端 Linux
// sshd 会在重连时自愈残留 socket。Pdeathsig 是尽力而为的收场信号（Go 的
// forking 线程退役时可能不触发），真正兜底的是 supervisor 的退避重启：
// server 崩溃后残留的 ssh 会被新实例的 bind 竞争挤掉或随远端自愈消亡。
func (m *ForwardManager) spawn(h *fwdHost) error {
	cmd := exec.Command(m.sshCommand, "-N",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "ConnectTimeout=10",
		"-R", h.remotePath+":"+m.sockPath,
		h.host,
	)
	withPdeathsig(cmd) // Linux：父死即 SIGTERM；其他平台 no-op（见 procattr_other.go）
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// stderr 走 pipe 时，Wait 会等所有持有写端的孙进程退出（如 sh 脚本
	// 的 sleep）；WaitDelay 给收割设上界，防止 stop 路径被孤儿孙进程拖住。
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn ssh: %w", err)
	}
	// Running/LastStart 只在 Start 成功后置位：Start 失败（如 PATH 无 ssh）
	// 时 List() 不应出现从未存在的"运行中"假阳性。
	h.setState(func(s *ForwardState) {
		s.Running = true
		s.LastStart = time.Now().Format(time.RFC3339)
	})
	m.notify() // Running=true 已生效：先推一帧，退出翻转时再推一帧
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			if tail := strings.TrimSpace(stderr.String()); tail != "" {
				if len(tail) > 200 {
					tail = tail[len(tail)-200:]
				}
				return fmt.Errorf("%v: %s", err, tail)
			}
			return err
		}
		return nil
	case <-h.stop:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return nil
	}
}

// loadConf 读取配置文件原始行（校验交给 parseForwardLine）。
func (m *ForwardManager) loadConf() []string {
	f, err := os.Open(m.confPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// rewriteConfLocked 要求调用方持有 m.mu，且必须全程持锁到落盘完成：
// 若中途放锁，并发的 Add 追加行会被本次整份重写截断丢失（内存 map
// 中尚无该主机），重启后通道静默消失。
func (m *ForwardManager) rewriteConfLocked() error {
	entries := make([]string, 0, len(m.hosts))
	for _, h := range m.hosts {
		entries = append(entries, canonicalLine(h.host, h.remotePath))
	}
	sort.Strings(entries)
	if err := os.MkdirAll(filepath.Dir(m.confPath), 0o700); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("# sudogate forward targets — one per line: host or host:/abs/path/sock\n")
	b.WriteString("# managed by `sudogate-server forward`; uid 非 1000 的远端用 host:/path 覆盖\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\n", e)
	}
	return os.WriteFile(m.confPath, b.Bytes(), 0o600)
}
