package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sudogate/internal/proto"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	dir := t.TempDir()
	s := New(priv, Options{
		SocketPath: filepath.Join(dir, "s.sock"),
		Timeout:    2 * time.Second,
		MaxPending: 5,
		StatePath:  filepath.Join(dir, "state.json"),
		AuditPath:  filepath.Join(dir, "audit.jsonl"),
	})
	return s, dir
}

func testReq(t *testing.T, cmd string) (*proto.Request, net.Conn, net.Conn) {
	t.Helper()
	var raw [16]byte
	rand.Read(raw[:])
	var epub [32]byte
	rand.Read(epub[:])
	req := &proto.Request{
		V: proto.Version, ID: hex.EncodeToString(raw[:]),
		Host: "mac-mini", User: "haotianyu",
		Command: cmd, CWD: "/home/haotianyu",
		TS:   time.Now().Unix(),
		EPub: base64.StdEncoding.EncodeToString(epub[:]),
	}
	c, s := net.Pipe()
	return req, c, s
}

func addEntry(t *testing.T, s *Server, cmd string) string {
	t.Helper()
	req, c, srvEnd := testReq(t, cmd)
	// 常驻 drain：否则 Approve 向 pipe 写响应会阻塞到 5s 写 deadline。
	go io.Copy(io.Discard, c)
	if err := s.addPending(req, srvEnd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srvEnd.Close() })
	return req.ID
}

func pendingCount(s *Server) int {
	return len(s.ListPending())
}

// 去合并：同 host+command 的两条请求必须是两个独立条目。
func TestNoMergeSameCommand(t *testing.T) {
	s, _ := newTestServer(t)
	addEntry(t, s, "apt update")
	addEntry(t, s, "apt update")
	if got := pendingCount(s); got != 2 {
		t.Fatalf("want 2 independent entries, got %d", got)
	}
}

// 批准一条不影响另一条。
func TestApproveOneLeavesOther(t *testing.T) {
	s, _ := newTestServer(t)
	a := addEntry(t, s, "apt update")
	addEntry(t, s, "apt update")
	if err := s.Approve(a, "pw"); err != nil {
		t.Fatal(err)
	}
	if got := pendingCount(s); got != 1 {
		t.Fatalf("want 1 remaining entry, got %d", got)
	}
}

// client 断连（Ctrl+C 场景）：条目应在断连后立刻消失，审计记 cancelled。
func TestHandleDataClientGone(t *testing.T) {
	s, dir := newTestServer(t)
	req, c, srvEnd := testReq(t, "id")
	go s.handleData(srvEnd)

	// 发请求帧，等条目出现。
	if err := proto.WriteFrame(c, req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return pendingCount(s) == 1 })

	// client 消失（对端 Close → server 探测读返回 → clientGone）。
	c.Close()
	waitFor(t, 2*time.Second, func() bool { return pendingCount(s) == 0 })

	// 审计应有一条 cancelled。
	b, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var ae auditEntry
	if err := json.Unmarshal([]byte(lastLine(string(b))), &ae); err != nil {
		t.Fatalf("audit line: %v", err)
	}
	if ae.Decision != "cancelled" || ae.ID != req.ID {
		t.Fatalf("audit = %+v, want cancelled %s", ae, req.ID)
	}
}

// clientGone 必须幂等：条目已被批准/拒绝/超时移除后再到为 no-op。
func TestClientGoneIdempotent(t *testing.T) {
	s, _ := newTestServer(t)
	id := addEntry(t, s, "id")
	if err := s.Deny(id); err != nil {
		t.Fatal(err)
	}
	s.clientGone(id) // 不得 panic 或复活条目
	s.clientGone("nonexistent")
	if got := pendingCount(s); got != 0 {
		t.Fatalf("want 0 entries, got %d", got)
	}
}

// 超时路径在去合并后仍工作（独立条目独立倒计时）。
func TestExpireIndependent(t *testing.T) {
	s, _ := newTestServer(t)
	addEntry(t, s, "id")
	time.Sleep(3 * time.Second) // Timeout = 2s
	if got := pendingCount(s); got != 0 {
		t.Fatalf("entry should have expired, got %d", got)
	}
}

// 批准后 client 已断：投递失败不阻塞队列清理。
func TestApproveAfterClientGone(t *testing.T) {
	s, _ := newTestServer(t)
	req, c, srvEnd := testReq(t, "id")
	if err := s.addPending(req, srvEnd); err != nil {
		t.Fatal(err)
	}
	c.Close()
	srvEnd.Close()
	if err := s.Approve(req.ID, "pw"); err != nil {
		t.Fatalf("approve on dead conn should still clean up: %v", err)
	}
	if got := pendingCount(s); got != 0 {
		t.Fatalf("want 0 entries, got %d", got)
	}
}

// 关机不再死等：走真 handleData 全路径——真连接发帧入队（探测读阻塞中，
// client 保持打开），SIGINT 后 closeAll 必须先解锁这些读再 Wait。
// 若有人把 closeAll 挪回 dataWG.Wait() 之后，本测试会超时失败。
func TestShutdownWithPending(t *testing.T) {
	s, dir := newTestServer(t)
	s.opt.SocketPath = filepath.Join(dir, "run.sock")
	s.opt.StatePath = filepath.Join(dir, "state.json")
	done := make(chan error, 1)
	go func() { done <- s.Run() }()
	waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(s.opt.SocketPath)
		return err == nil
	})

	req, _, _ := testReq(t, "id")
	c, err := net.Dial("unix", s.opt.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := proto.WriteFrame(c, req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return pendingCount(s) == 1 })

	// client 全程保持连接：唯一能让探测读返回的是 server 侧 closeAll。
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down promptly with a blocked probe read")
	}
}

// closeAll 之后的迟到握手必须被拒：不入队、不复活队列（shutdown 竞态防护）。
func TestAddPendingAfterShutdown(t *testing.T) {
	s, _ := newTestServer(t)
	addEntry(t, s, "id")
	s.closeAll()
	req, _, srvEnd := testReq(t, "late")
	defer srvEnd.Close()
	if err := s.addPending(req, srvEnd); err == nil {
		t.Fatal("addPending after closeAll should be rejected")
	}
	if got := pendingCount(s); got != 0 {
		t.Fatalf("want 0 entries, got %d", got)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
