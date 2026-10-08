package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubSSH 生成一个长眠脚本冒充 ssh，避免测试真连 DNS/网络。
func stubSSH(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stub-ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestManager(t *testing.T) *ForwardManager {
	t.Helper()
	m := NewForwardManager(filepath.Join(t.TempDir(), "forward.conf"), "/tmp/none.sock")
	m.sshCommand = stubSSH(t)
	return m
}

func TestForwardDelay(t *testing.T) {
	for i, want := range forwardDelays {
		if got := forwardDelay(i); got != want {
			t.Fatalf("forwardDelay(%d) = %v, want %v", i, got, want)
		}
	}
	if got := forwardDelay(100); got != forwardDelays[len(forwardDelays)-1] {
		t.Fatalf("封顶失败: %v", got)
	}
	if got := forwardDelay(-3); got != forwardDelays[0] {
		t.Fatalf("负数应回退首档: %v", got)
	}
}

func TestParseForwardLine(t *testing.T) {
	cases := []struct {
		in         string
		host, path string
		ok         bool
	}{
		{"mac-mini", "mac-mini", defaultRemoteSock, true},
		{" mac-mini ", "mac-mini", defaultRemoteSock, true},
		{"nuc:/run/user/1001/sudogate.sock", "nuc", "/run/user/1001/sudogate.sock", true},
		{"user@10.0.0.5:/tmp/sg.sock", "user@10.0.0.5", "/tmp/sg.sock", true},
		{"user@host:22", "user@host:22", defaultRemoteSock, true}, // 冒号后非 / 不拆
		{"", "", "", false},
		{"a b", "", "", false},
		{"#nuc", "", "", false},  // 无空格注释行
		{"host:", "", "", false}, // 尾冒号无路径是畸形目标
		{":/only/path", "", "", false},
	}
	for _, c := range cases {
		h, p, ok := parseForwardLine(c.in)
		if ok != c.ok || h != c.host || p != c.path {
			t.Fatalf("parseForwardLine(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, h, p, ok, c.host, c.path, c.ok)
		}
	}
}

func TestForwardConfRoundTrip(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "forward.conf")
	os.WriteFile(conf, []byte("# 注释\nhost-a\n\nhost-b:/run/user/1001/sudogate.sock\nbad line with spaces\n"), 0o600)

	m := NewForwardManager(conf, "/tmp/none.sock")
	m.sshCommand = stubSSH(t)
	defer m.Shutdown()
	var hosts []string
	for _, line := range m.loadConf() {
		if h, _, ok := parseForwardLine(line); ok {
			hosts = append(hosts, h)
		}
	}
	// 非法行被 parseForwardLine 过滤；合法两台保留
	if len(hosts) != 2 || hosts[0] != "host-a" || hosts[1] != "host-b" {
		t.Fatalf("loadConf+parse 过滤错误: %v", hosts)
	}

	if err := m.Add("host-c"); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, line := range m.loadConf() {
		if h, _, ok := parseForwardLine(line); ok && h == "host-c" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("Add 后应恰好 1 条 host-c, got %d", found)
	}
}

func TestForwardRemoveRewritesConf(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	m.startHost("host-a", defaultRemoteSock)
	m.startHost("host-b", "/run/user/1001/sudogate.sock")
	if err := m.Remove("host-a"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(m.confPath)
	s := string(data)
	if strings.Contains(s, "host-a\n") || !strings.Contains(s, "host-b:/run/user/1001/sudogate.sock") {
		t.Fatalf("rewriteConf 应剔除 host-a 并保留 host-b 的路径覆盖: %q", s)
	}
}

func TestForwardRemoveNormalizes(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	m.startHost("host-a", defaultRemoteSock)
	// 带空白参数也必须命中（否则假成功、重启后复活）
	if err := m.Remove(" host-a "); err != nil {
		t.Fatalf("Remove 带空白应规范化成功: %v", err)
	}
	m.mu.Lock()
	n := len(m.hosts)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("Remove 后 hosts 应空, got %d", n)
	}
}

func TestForwardSuperviseStop(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	m.startHost("h1", defaultRemoteSock)

	done := make(chan struct{})
	go func() { m.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown 卡死")
	}
}

func TestForwardAddInvalid(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	for _, bad := range []string{"", "a b", "a\tb", ":/only/path"} {
		if err := m.Add(bad); err == nil {
			t.Fatalf("Add(%q) 应报错", bad)
		}
	}
}

func TestForwardAddIdempotent(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	if err := m.Add("dup"); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("dup"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(m.confPath)
	if got := strings.Count(string(data), "dup"); got != 1 {
		t.Fatalf("幂等失败，dup 出现 %d 次", got)
	}
}

func TestForwardAddWithPathOverride(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	if err := m.Add("nuc:/run/user/1001/sudogate.sock"); err != nil {
		t.Fatal(err)
	}
	// 关键回归：Add 落盘必须是规范化行（host:/path），否则重启后
	// Start() 按裸 host 解析，覆盖静默丢失。直接断言磁盘内容，
	// 不依赖任何会触发 rewriteConf 的副作用。
	data, err := os.ReadFile(m.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "nuc:/run/user/1001/sudogate.sock") {
		t.Fatalf("Add 落盘丢失路径覆盖: %q", string(data))
	}
	m.mu.Lock()
	h := m.hosts["nuc"]
	m.mu.Unlock()
	if h == nil || h.remotePath != "/run/user/1001/sudogate.sock" {
		t.Fatalf("路径覆盖未生效: %+v", h)
	}
	// Remove 后回写同样保留覆盖语法
	if err := m.Add("spare"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("spare"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(m.confPath)
	if !strings.Contains(string(data), "nuc:/run/user/1001/sudogate.sock") {
		t.Fatalf("回写应保留 host:path 形式: %q", string(data))
	}
}

// onChange 触发测试：stub ssh 启动与被收割各推一帧；回调在锁外执行
// （回调内取 m.mu 不死锁即验证了锁序契约）。
func TestForwardOnChangeFired(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	fired := make(chan struct{}, 16)
	m.SetOnChange(func() {
		m.mu.Lock() // 回调里反向取 m.mu：若 notify 持锁即死锁（测试会卡死暴露）
		m.mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	m.startHost("alpha", defaultRemoteSock)
	// stub spawn 成功 → 至少一帧
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("spawn 后应触发 onChange")
	}
	m.mu.Lock()
	h := m.hosts["alpha"]
	m.mu.Unlock()
	if h == nil {
		t.Fatal("alpha 应在运行")
	}
}

func TestForwardAddPathConflictErrors(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	if err := m.Add("nuc"); err != nil {
		t.Fatal(err)
	}
	// 同 host 换路径：必须显式报错，静默沿用旧路径会让覆盖流程失效
	err := m.Add("nuc:/run/user/1001/sudogate.sock")
	if err == nil {
		t.Fatal("换路径的 Add 应报错")
	}
	if !strings.Contains(err.Error(), "forward remove nuc") {
		t.Fatalf("报错应给出操作指引: %v", err)
	}
	// 同 host 同路径：幂等 nil
	if err := m.Add("nuc"); err != nil {
		t.Fatalf("同路径幂等应 nil: %v", err)
	}
}

func TestForwardListSnapshotDeterministic(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	m.startHost("alpha", defaultRemoteSock)
	// stub ssh 起来后 Running 应稳定为 true（不再依赖真 ssh 的时序）
	deadline := time.Now().Add(2 * time.Second)
	for {
		list := m.List()
		if len(list) != 1 || list[0].Host != "alpha" {
			t.Fatalf("List 快照错误: %+v", list)
		}
		if list[0].Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stub ssh 应在 2s 内进入 Running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 并发 List/Add/Remove 不死锁不竞态（-race 检测）。
func TestForwardConcurrent(t *testing.T) {
	m := newTestManager(t)
	defer m.Shutdown()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Add("x")
			_ = m.List()
			_ = m.Remove("x")
		}()
	}
	wg.Wait()
}
