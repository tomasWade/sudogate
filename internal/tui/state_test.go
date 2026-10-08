package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sudogate.state")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadState(t *testing.T) {
	p := writeTemp(t, `{"updated":"2026-10-07T10:00:00Z","timeout_sec":90,"pending":[
		{"id":"aabb","host":"h1","user":"u1","command":"pacman -Syu","cwd":"/tmp","age_sec":5}]}`)
	st, err := ReadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.TimeoutSec != 90 {
		t.Fatalf("timeout_sec = %d, want 90", st.TimeoutSec)
	}
	if len(st.Pending) != 1 || st.Pending[0].Command != "pacman -Syu" {
		t.Fatalf("pending 解析错误: %+v", st.Pending)
	}
}

func TestReadStateDefaults(t *testing.T) {
	p := writeTemp(t, `{"updated":"2026-10-07T10:00:00Z","pending":[]}`)
	st, err := ReadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.TimeoutSec != 120 {
		t.Fatalf("timeout_sec 默认应回 120, got %d", st.TimeoutSec)
	}
}

func TestReadStateMissing(t *testing.T) {
	if _, err := ReadState(filepath.Join(t.TempDir(), "nope.state")); err == nil {
		t.Fatal("缺失文件应报错")
	}
}

func TestReadStateCorrupt(t *testing.T) {
	p := writeTemp(t, `{not-json`)
	if _, err := ReadState(p); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestReadStateWithForwards(t *testing.T) {
	p := writeTemp(t, `{"updated":"2026-10-07T10:00:00Z","timeout_sec":90,"forwards":[
		{"host":"mac-mini","remote_path":"/run/user/1000/sudogate.sock","running":true,"restarts":0},
		{"host":"nuc","running":false,"restarts":3,"last_err":"ssh: connect timeout"}],"pending":[]}`)
	st, err := ReadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Forwards) != 2 {
		t.Fatalf("forwards 解析错误: %+v", st.Forwards)
	}
	if !st.Forwards[0].Running || st.Forwards[0].Host != "mac-mini" {
		t.Fatalf("mac-mini 应为 running: %+v", st.Forwards[0])
	}
	if st.Forwards[1].Running || st.Forwards[1].Restarts != 3 || st.Forwards[1].LastErr != "ssh: connect timeout" {
		t.Fatalf("nuc 状态错误: %+v", st.Forwards[1])
	}
	// 无 forwards 字段时默认空数组（向后兼容）
	st2, err := ReadState(writeTemp(t, `{"updated":"2026-10-07T10:00:00Z","pending":[]}`))
	if err != nil || len(st2.Forwards) != 0 {
		t.Fatalf("缺省 forwards 应为空: %+v err=%v", st2.Forwards, err)
	}
}

func TestRemainingSec(t *testing.T) {
	st := &State{
		Updated:    time.Now().Add(-10 * time.Second),
		TimeoutSec: 120,
		Pending:    []PendingEntry{{ID: "x", AgeSec: 30}},
	}
	// updated 距今 10s，age 30s，timeout 120s → 剩余 80s（±2s 时钟容差）
	if got := st.RemainingSec(&st.Pending[0]); got < 78 || got > 82 {
		t.Fatalf("RemainingSec = %d, want ~80", got)
	}
}
