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
}

type pendingConn struct {
	req  proto.Request
	conn net.Conn
}

type Entry struct {
	ID      string    `json:"id"`
	Host    string    `json:"host"`
	User    string    `json:"user"`
	Command string    `json:"command"`
	CWD     string    `json:"cwd"`
	Created time.Time `json:"created"`
	conns   []*pendingConn
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
	Conns      int    `json:"conns"`
}

type Server struct {
	opt  Options
	priv ed25519.PrivateKey

	mu       sync.Mutex
	entries  map[string]*Entry
	mergeIdx map[string]string
}

func New(priv ed25519.PrivateKey, opt Options) *Server {
	if opt.Timeout <= 0 {
		opt.Timeout = 120 * time.Second
	}
	if opt.MaxPending <= 0 {
		opt.MaxPending = 5
	}
	return &Server{
		opt:      opt,
		priv:     priv,
		entries:  map[string]*Entry{},
		mergeIdx: map[string]string{},
	}
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
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
	dataWG.Wait()
	ctlWG.Wait()
	s.closeAll()
	return nil
}

func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		for _, pc := range e.conns {
			pc.conn.Close()
		}
		e.timer.Stop()
	}
	s.entries = map[string]*Entry{}
	s.mergeIdx = map[string]string{}
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
}

func (s *Server) addPending(req *proto.Request, conn net.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := req.Host + "\x00" + req.Command
	if id, ok := s.mergeIdx[key]; ok {
		e := s.entries[id]
		e.conns = append(e.conns, &pendingConn{req: *req, conn: conn})
		return nil
	}
	if len(s.entries) >= s.opt.MaxPending {
		return errors.New("busy")
	}
	id := req.ID
	e := &Entry{
		ID: id, Host: req.Host, User: req.User,
		Command: req.Command, CWD: req.CWD,
		Created: time.Now(),
		conns:   []*pendingConn{{req: *req, conn: conn}},
	}
	e.timer = time.AfterFunc(s.opt.Timeout, func() { s.expire(id) })
	s.entries[id] = e
	s.mergeIdx[key] = id
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
	idBytes, err := hex.DecodeString(id)
	if err != nil {
		s.finishLocked(e, proto.ReasonDenied, "denied")
		return err
	}
	_ = idBytes
	for _, pc := range e.conns {
		pcID, err := hex.DecodeString(pc.req.ID)
		if err != nil {
			continue
		}
		epub, err := base64.StdEncoding.DecodeString(pc.req.EPub)
		if err != nil {
			continue
		}
		sl, err := seal.SealPassword(epub, pcID, pc.req.Command, password)
		if err != nil {
			continue
		}
		sig := seal.Sign(s.priv, pcID, pc.req.Command, epub)
		resp := &proto.Response{
			V: proto.Version, OK: true, ID: pc.req.ID,
			Seal: sl,
			Sig:  base64.StdEncoding.EncodeToString(sig),
		}
		pc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		proto.WriteFrame(pc.conn, resp)
		pc.conn.Close()
	}
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
		for _, pc := range e.conns {
			pc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			proto.WriteFrame(pc.conn, &proto.Response{V: proto.Version, OK: false, ID: e.ID, Reason: reason})
			pc.conn.Close()
		}
	}
	e.timer.Stop()
	delete(s.entries, e.ID)
	delete(s.mergeIdx, e.Host+"\x00"+e.Command)
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
		Updated time.Time   `json:"updated"`
		Pending []EntryInfo `json:"pending"`
	}
	pending := make([]EntryInfo, 0, len(s.entries))
	for _, e := range s.entries {
		pending = append(pending, e.info())
	}
	b, _ := json.Marshal(state{Updated: time.Now(), Pending: pending})
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
		Conns:      len(e.conns),
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
