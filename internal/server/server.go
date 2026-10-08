package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"sudogate/internal/proto"
	"sudogate/internal/seal"
)

type Options struct {
	SocketPath string
	Timeout    time.Duration
	MaxPending int
	StatePath  string
	AuditPath  string
	// ForwardConfPath 非空时启用内嵌转发管理（专用 ssh -N -R 通道）。
	ForwardConfPath string
	// PopupConfPath 非空时启用桌面弹窗审批（配置即终端，见 PopupManager）。
	PopupConfPath string
}

type Entry struct {
	ID      string    `json:"id"`
	Host    string    `json:"host"`
	User    string    `json:"user"`
	Command string    `json:"command"`
	CWD     string    `json:"cwd"`
	Created time.Time `json:"created"`
	epub    string    // client 的临时公钥（base64），批准时封装密码
	conn    net.Conn  // 请求方连接；断连即撤销本条（clientGone）
	timer   *time.Timer
}

type EntryInfo struct {
	ID      string `json:"id"`
	Host    string `json:"host"`
	User    string `json:"user"`
	Command string `json:"command"`
	CWD     string `json:"cwd"`
	AgeSec  int64  `json:"age_sec"`
}

func (e *Entry) info() EntryInfo {
	return EntryInfo{
		ID: e.ID, Host: e.Host, User: e.User,
		Command: e.Command, CWD: e.CWD,
		AgeSec: int64(time.Since(e.Created) / time.Second),
	}
}

type auditEntry struct {
	TS         string `json:"ts"`
	ID         string `json:"id"`
	Host       string `json:"host"`
	User       string `json:"user"`
	Command    string `json:"command"`
	CWD        string `json:"cwd"`
	Decision   string `json:"decision"`
	DurationMS int64  `json:"duration_ms"`
}

type Server struct {
	opt   Options
	priv  ed25519.PrivateKey
	fwd   *ForwardManager
	popup *PopupManager

	mu           sync.Mutex
	entries      map[string]*Entry
	shuttingDown bool // closeAll 后置位：迟到的握手连接不再入队（见 addPending）
}

func New(priv ed25519.PrivateKey, opt Options) *Server {
	if opt.Timeout <= 0 {
		opt.Timeout = 120 * time.Second
	}
	if opt.MaxPending <= 0 {
		opt.MaxPending = 5
	}
	s := &Server{
		opt:     opt,
		priv:    priv,
		entries: map[string]*Entry{},
	}
	if opt.ForwardConfPath != "" {
		s.fwd = NewForwardManager(opt.ForwardConfPath, opt.SocketPath)
		// 转发状态翻转 → 重写 state：TUI/面板经 fsnotify 拿到健康灯。
		// notifyState 会经 fwd.List() 回取 fwd.mu，因此回调只会在
		// manager 无锁时被调（Add/Remove/supervise 均已保证锁外 notify）。
		s.fwd.SetOnChange(s.notifyState)
	}
	if opt.PopupConfPath != "" {
		s.popup = NewPopupManager(opt.PopupConfPath, "")
	}
	return s
}

// notifyState 重写一次 state 文件（转发状态变化等来源调用）。
func (s *Server) notifyState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeStateLocked()
}

func (s *Server) Run() error {
	os.Remove(s.opt.SocketPath)
	os.Remove(CtlPath(s.opt.SocketPath))
	ln, err := net.Listen("unix", s.opt.SocketPath)
	if err != nil {
		return err
	}
	ctlLn, err := net.Listen("unix", CtlPath(s.opt.SocketPath))
	if err != nil {
		ln.Close()
		return err
	}
	os.Chmod(s.opt.SocketPath, 0o600)
	os.Chmod(CtlPath(s.opt.SocketPath), 0o600)

	// 启动即落一次空 state：让 TUI/面板在空闲期也能确认 server 存活，
	// 否则文件只在首条请求后才出现，"读不到"会被误读为"未运行"。
	s.mu.Lock()
	s.writeStateLocked()
	s.mu.Unlock()

	// 内嵌转发管理：拉起各主机的专用 ssh -N -R 通道（子进程由 Pdeathsig
	// 与 Shutdown 双重看护，随 server 生命周期同生共死）。
	if s.fwd != nil {
		s.fwd.Start()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh) // 测试内多次 Run/停启时不残留全局信号注册
	go func() {
		<-sigCh
		ln.Close()
		ctlLn.Close()
	}()
	fmt.Fprintf(os.Stderr, "sudogate-server: listening %s (ctl %s, timeout %s, max %d)\n",
		s.opt.SocketPath, CtlPath(s.opt.SocketPath), s.opt.Timeout, s.opt.MaxPending)

	var dataWG sync.WaitGroup
	var ctlWG sync.WaitGroup
	go func() {
		for {
			c, err := ctlLn.Accept()
			if err != nil {
				return
			}
			ctlWG.Add(1)
			go func() { defer ctlWG.Done(); s.handleCtl(c) }()
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		dataWG.Add(1)
		go func() { defer dataWG.Done(); s.handleData(c) }()
	}
	// 必须在 dataWG.Wait() 之前关闭所有 pending 连接：handleData 此刻
	// 阻塞在取消探测读上，只有关掉对端连接才能让它们返回（clientGone
	// 对已清空的 entries 幂等 no-op）。否则有 pending 时收到 SIGINT 会
	// 死等各 client 自行断连（最长 120s），被 systemd SIGKILL 后丢掉
	// 转发下线、最终 state 与审计。
	s.closeAll()
	dataWG.Wait()
	ctlWG.Wait()
	if s.fwd != nil {
		s.fwd.Shutdown()
	}
	return nil
}

func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuttingDown = true
	// 关机丢弃也要留审计痕：否则 audit.jsonl 里无法区分"server 停机时
	// 还有未决请求"与"请求从未发生"。
	for _, e := range s.entries {
		e.conn.Close()
		e.timer.Stop()
		s.appendAudit(e, "shutdown")
	}
	s.entries = map[string]*Entry{}
	s.writeStateLocked()
}

func (s *Server) handleData(conn net.Conn) {
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return
	}
	var req proto.Request
	if err := proto.ReadFrame(conn, &req); err != nil {
		conn.Close()
		return
	}
	if err := proto.ValidateRequest(&req, time.Now().Unix()); err != nil {
		proto.WriteFrame(conn, &proto.Response{V: proto.Version, OK: false, ID: req.ID, Reason: proto.ReasonMalformed})
		conn.Close()
		return
	}
	if err := s.addPending(&req, conn); err != nil {
		proto.WriteFrame(conn, &proto.Response{V: proto.Version, OK: false, ID: req.ID, Reason: proto.ReasonBusy})
		conn.Close()
		return
	}
	// 桌面弹窗：必须在 s.mu 之外调 Notify（PopupManager 有自己的锁）。
	// 读长度与 Notify 之间若有并发入队，多算的条目无害——去重靠弹窗
	// 进程探测，不靠精确计数。
	if s.popup != nil {
		s.mu.Lock()
		n := len(s.entries)
		s.mu.Unlock()
		s.popup.Notify(n)
	}
	// 取消感知：askpass 协议里 client 发完请求帧后不会再发任何数据，
	// 所以本连接上任何 Read 返回（EOF/RST/数据/超时）都意味着 client
	// 已消失——远端 sudo 被 Ctrl+C、askpass 被杀等。阻塞发生在本连接
	// 专属的 goroutine 里，不影响其他请求的并发处理。必须先清掉握手期
	// 的 10s ReadDeadline，否则会把 deadline 到期误判为 client 断连；
	// 清 deadline 失败（实践不可达）则放弃探测——条目交由审批/超时收尾，
	// 不能在此直接撤销（否则刚入队就自撤）。
	if conn.SetReadDeadline(time.Time{}) == nil {
		var b [1]byte
		conn.Read(b[:])
		s.clientGone(req.ID)
	}
}

// clientGone 撤销一条因请求方断连而失效的 pending（幂等：条目已被
// 批准/拒绝/超时移除时为 no-op，与 Approve/Deny/expire 的竞态由 s.mu
// 串行化，先到先生效）。
func (s *Server) clientGone(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return
	}
	e.conn.Close()
	e.timer.Stop()
	delete(s.entries, id)
	s.writeStateLocked()
	s.appendAudit(e, "cancelled")
}

func (s *Server) addPending(req *proto.Request, conn net.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 关机竞态防护：信号到达后 closeAll 已清空队列，但一个在关机前
	// 刚被 accept、还在 10s 握手窗内的连接此刻才完成读帧——若放它
	// 入队，其取消探测读会让 dataWG.Wait() 再死等一个完整超时周期。
	// 关机中一律拒绝（client 收 busy 响应后退出）。
	if s.shuttingDown {
		return errors.New("busy")
	}
	// 刻意不做同 host+command 合并：认证可以共享，但命令执行不能并行
	// （apt/dpkg 等文件锁会让并行放行的一半直接失败）。每条请求独立
	// 条目、独立倒计时，人工逐条批准的节奏天然把执行串行化。
	if len(s.entries) >= s.opt.MaxPending {
		return errors.New("busy")
	}
	id := req.ID
	e := &Entry{
		ID: id, Host: req.Host, User: req.User,
		Command: req.Command, CWD: req.CWD,
		Created: time.Now(),
		epub:    req.EPub,
		conn:    conn,
	}
	e.timer = time.AfterFunc(s.opt.Timeout, func() { s.expire(id) })
	s.entries[id] = e
	s.writeStateLocked()
	return nil
}

func (s *Server) expire(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return
	}
	s.finishLocked(e, proto.ReasonTimeout, "timeout")
}

func (s *Server) Approve(id, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return fmt.Errorf("no pending request %s", id)
	}
	idBytes, err := hex.DecodeString(e.ID)
	if err != nil {
		s.finishLocked(e, proto.ReasonDenied, "denied")
		return err
	}
	epub, err := base64.StdEncoding.DecodeString(e.epub)
	if err != nil {
		s.finishLocked(e, proto.ReasonDenied, "denied")
		return err
	}
	sl, err := seal.SealPassword(epub, idBytes, e.Command, password)
	if err != nil {
		s.finishLocked(e, proto.ReasonDenied, "denied")
		return err
	}
	sig := seal.Sign(s.priv, idBytes, e.Command, epub)
	resp := &proto.Response{
		V: proto.Version, OK: true, ID: e.ID,
		Seal: sl,
		Sig:  base64.StdEncoding.EncodeToString(sig),
	}
	e.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	proto.WriteFrame(e.conn, resp)
	e.conn.Close()
	s.finishLocked(e, "", "approved")
	return nil
}

func (s *Server) Deny(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return fmt.Errorf("no pending request %s", id)
	}
	s.finishLocked(e, proto.ReasonDenied, "denied")
	return nil
}

func (s *Server) finishLocked(e *Entry, reason, decision string) {
	if reason != "" {
		e.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		proto.WriteFrame(e.conn, &proto.Response{V: proto.Version, OK: false, ID: e.ID, Reason: reason})
		e.conn.Close()
	}
	e.timer.Stop()
	delete(s.entries, e.ID)
	s.writeStateLocked()
	s.appendAudit(e, decision)
}

func (s *Server) ListPending() []EntryInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EntryInfo, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.info())
	}
	return out
}

func (s *Server) writeStateLocked() {
	if s.opt.StatePath == "" {
		return
	}
	type state struct {
		Updated    time.Time      `json:"updated"`
		TimeoutSec int64          `json:"timeout_sec"`
		Forwards   []ForwardState `json:"forwards,omitempty"`
		Pending    []EntryInfo    `json:"pending"`
	}
	pending := make([]EntryInfo, 0, len(s.entries))
	for _, e := range s.entries {
		pending = append(pending, e.info())
	}
	var forwards []ForwardState
	if s.fwd != nil {
		forwards = s.fwd.List()
	}
	b, _ := json.Marshal(state{Updated: time.Now(), TimeoutSec: int64(s.opt.Timeout / time.Second), Forwards: forwards, Pending: pending})
	// 原地写（truncate+write），不能用 tmp+rename 原子写：omarchy 面板的
	// inotifywait watch 的是本文件的 inode，rename 换 inode 会让它在
	// move_self 事件后永久失聪（进程不退出、也不触发重试）。半读窗口由
	// 各消费者防御：TUI 有 readDirty 保底，面板 cat 小文件窗口极小。
	os.MkdirAll(filepath.Dir(s.opt.StatePath), 0o700)
	os.WriteFile(s.opt.StatePath, b, 0o600)
}

func (s *Server) appendAudit(e *Entry, decision string) {
	if s.opt.AuditPath == "" {
		return
	}
	entry := auditEntry{
		TS: time.Now().Format(time.RFC3339), ID: e.ID, Host: e.Host,
		User: e.User, Command: e.Command, CWD: e.CWD,
		Decision:   decision,
		DurationMS: time.Since(e.Created).Milliseconds(),
	}
	b, _ := json.Marshal(entry)
	os.MkdirAll(filepath.Dir(s.opt.AuditPath), 0o700)
	f, err := os.OpenFile(s.opt.AuditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}
