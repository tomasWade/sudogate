package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTUI 启动即往计数文件 append 一行再睡住：数行数即数 spawn 次数。
func writeFakeTUI(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-tui.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho x >> \""+filepath.Join(dir, "spawns.log")+"\"\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func spawnCount(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "spawns.log"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func waitCond(t *testing.T, d time.Duration, cond func() bool) {
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

// conf 缺失 = 关闭：Notify 不 spawn 任何东西。
func TestPopupDisabledWithoutConf(t *testing.T) {
	dir := t.TempDir()
	p := NewPopupManager(filepath.Join(dir, "none.conf"), writeFakeTUI(t, dir))
	p.Notify(1)
	_, _, open := p.Status()
	if open {
		t.Fatal("conf 缺失时不应弹窗")
	}
}

// 队列空不弹；非空弹；弹窗活着期间去重（真计数）；退出后可重弹。
func TestPopupLifecycle(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "popup.conf")
	if err := os.WriteFile(conf, []byte("env "+filepath.Join(dir, "fake-tui.sh")+" {tui} extra-arg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewPopupManager(conf, writeFakeTUI(t, dir))

	p.Notify(0) // 空队列
	_, _, open := p.Status()
	if open {
		t.Fatal("空队列不应弹窗")
	}

	p.Notify(1)
	waitCond(t, 2*time.Second, func() bool {
		_, _, open := p.Status()
		return open
	})

	// 弹窗活着（fake-tui sleep 5 未退）：并发触发 5 次也不得二次 spawn
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.Notify(2) }()
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond) // 给潜在的错误 spawn 留出落盘时间
	if n := spawnCount(t, dir); n != 1 {
		t.Fatalf("弹窗活着期间应去重（spawn 1 次），实际 %d 次", n)
	}
}

// {tui} 必须独立成词且必须存在；违反任一规则都不得弹窗。
func TestPopupTemplateTokenRule(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "popup.conf")
	if err := os.WriteFile(conf, []byte("echo {tui}x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewPopupManager(conf, writeFakeTUI(t, dir))
	p.Notify(1)
	time.Sleep(200 * time.Millisecond)
	_, _, open := p.Status()
	if open {
		t.Fatal("坏模板（{tui} 非独立词）不应弹窗")
	}
	// 缺失 {tui}：终端会裸跑（窗口不随清空自灭）——必须拒绝
	if err := os.WriteFile(conf, []byte("foot --app-id x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Notify(1)
	time.Sleep(200 * time.Millisecond)
	if _, _, open := p.Status(); open {
		t.Fatal("缺 {tui} 的模板不应弹窗")
	}
	if _, err := expandTemplate("a b", "/x/tui"); err == nil {
		t.Fatal("缺 {tui} 应报错")
	}
	// 展开函数直接验证
	args, err := expandTemplate("a {tui} b", "/x/tui")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "/x/tui", "--until-empty", "b"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}

// 弹窗退出后（cmd 被回收）可重弹。
func TestPopupRespawnAfterExit(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "popup.conf")
	if err := os.WriteFile(conf, []byte("env "+filepath.Join(dir, "fake-tui.sh")+" {tui}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewPopupManager(conf, writeFakeTUI(t, dir))
	p.Notify(1)
	waitCond(t, 2*time.Second, func() bool {
		_, _, open := p.Status()
		return open
	})
	waitCond(t, 8*time.Second, func() bool {
		_, _, open := p.Status()
		return !open // sleep 5 结束，Wait 回收，窗口关
	})
	p.Notify(1) // 关了就该能重弹
	// 等 spawn 计数落盘（Status open 只反映 Start 成功，echo 是异步的）
	waitCond(t, 2*time.Second, func() bool { return spawnCount(t, dir) == 2 })
}

// 注释行剥离：直接复制 example 文件（大量 # 注释 + 一行裸 kitty 模板）
// 必须开箱即用——注释变 argv 是静默失效（spawn 永远失败在 "#" 上）。
func TestPopupTemplateSkipsComments(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "popup.conf")
	content := "# sudogate 桌面弹窗审批配置（示例）\n# 复制到 ~/.config/sudogate/popup.conf 即启用\n#\n# kitty（默认）：\n" + DefaultPopupTemplate + "\n#\n# foot：\n# foot --app-id sudogate-approve -e {tui}\n"
	if err := os.WriteFile(conf, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewPopupManager(conf, writeFakeTUI(t, dir))
	if tpl := p.template(); tpl != DefaultPopupTemplate {
		t.Fatalf("注释应被剥离，got %q", tpl)
	}
	// 纯注释文件 = 关闭
	if err := os.WriteFile(conf, []byte("# 只有注释\n# 没有命令\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tpl := p.template(); tpl != "" {
		t.Fatalf("纯注释文件应视为关闭，got %q", tpl)
	}
}

// 开关文件操作：on 幂等（不覆盖用户模板）、off 删除。
func TestPopupOnOff(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "popup.conf")
	if err := PopupOn(conf, "kitty {tui}"); err != nil {
		t.Fatal(err)
	}
	if err := PopupOn(conf, "user-template {tui}"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(conf)
	if string(b) != "kitty {tui}\n" {
		t.Fatalf("PopupOn 不得覆盖已有模板: %q", b)
	}
	if err := PopupOff(conf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Fatal("PopupOff 应删除 conf")
	}
	if err := PopupOff(conf); err != nil {
		t.Fatal("PopupOff 幂等失败")
	}
}
