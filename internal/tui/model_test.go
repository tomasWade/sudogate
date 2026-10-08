package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func testModel(t *testing.T) Model {
	t.Helper()
	m := New("/tmp/none.state", "/tmp/none.ctl")
	m.state = &State{
		Updated:    time.Now(),
		TimeoutSec: 120,
		Pending: []PendingEntry{
			{ID: "e1", Host: "h1", User: "u1", Command: "cmd1", AgeSec: 0},
			{ID: "e2", Host: "h2", User: "u2", Command: "cmd2", AgeSec: 10},
			{ID: "e3", Host: "h3", User: "u3", Command: "cmd3", AgeSec: 20},
		},
	}
	return m
}

func step(m Model, key string) Model {
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return nm.(Model)
}

func stepSpecial(m Model, keyType tea.KeyType) Model {
	nm, _ := m.Update(tea.KeyMsg{Type: keyType})
	return nm.(Model)
}

func TestCursorNav(t *testing.T) {
	m := testModel(t)
	if m.cursor != 0 {
		t.Fatalf("初始 cursor = %d, want 0", m.cursor)
	}
	m = step(m, "j")
	if m.cursor != 1 {
		t.Fatalf("j 后 cursor = %d, want 1", m.cursor)
	}
	m = step(m, "j") // 第二次 → 2
	if m.cursor != 2 {
		t.Fatalf("jj 后 cursor = %d, want 2", m.cursor)
	}
	m = step(m, "j") // 第三次，环绕 → 0
	if m.cursor != 0 {
		t.Fatalf("末尾 j 应环绕到 0, got %d", m.cursor)
	}
	m = step(m, "k") // 反向环绕
	if m.cursor != 2 {
		t.Fatalf("顶部 k 应环绕到 2, got %d", m.cursor)
	}
	m = step(m, "g")
	if m.cursor != 0 {
		t.Fatalf("g 后 cursor = %d, want 0", m.cursor)
	}
}

func TestEmptyListIgnoresNav(t *testing.T) {
	m := testModel(t)
	m.state.Pending = nil
	m = step(m, "j")
	if m.cursor != 0 {
		t.Fatalf("空列表 j 后 cursor = %d, want 0", m.cursor)
	}
	m = step(m, "r")
	if m.mode != modeList {
		t.Fatal("空列表 r 不应进入动作")
	}
}

func TestPasswordModalFlow(t *testing.T) {
	m := testModel(t)
	m = stepSpecial(m, tea.KeyEnter)
	if m.mode != modePassword {
		t.Fatal("enter 应进入密码弹框")
	}
	m = stepSpecial(m, tea.KeyEsc)
	if m.mode != modeList {
		t.Fatal("esc 应回到列表")
	}
	if m.pwInput.Value() != "" {
		t.Fatal("esc 后密码应清空")
	}
}

func TestShrinkKeepsCursor(t *testing.T) {
	m := testModel(t)
	m.cursor = 2
	m.state.Pending = m.state.Pending[:1]
	nm, _ := m.Update(stateChangedMsg{st: m.state})
	got := nm.(Model)
	if got.cursor != 0 {
		t.Fatalf("列表收缩后 cursor = %d, want 0", got.cursor)
	}
}

func TestPasswordTargetVanishesCancelsInput(t *testing.T) {
	m := testModel(t)
	m = stepSpecial(m, tea.KeyEnter)
	if m.mode != modePassword || m.pwTarget != "e1" {
		t.Fatalf("enter 后应锚定 e1，got mode=%d target=%q", m.mode, m.pwTarget)
	}
	// 选中项过期被移除（e1 消失），弹框应自动取消而不是漂移到别的请求
	m.state.Pending = m.state.Pending[1:]
	nm, _ := m.Update(stateChangedMsg{st: m.state, serverUp: true})
	got := nm.(Model)
	if got.mode != modeList {
		t.Fatal("目标失效后应退出密码模式")
	}
	if got.pwInput.Value() != "" || got.pwTarget != "" {
		t.Fatal("目标失效后应清空密码与锚定")
	}
	if !got.noteIsEr {
		t.Fatal("应有取消提示")
	}
}

func TestPasswordEnterUsesAnchoredID(t *testing.T) {
	m := testModel(t)
	m = stepSpecial(m, tea.KeyEnter)
	m.pwInput.SetValue("secret")
	// 键入期间队列滚动：e1 挪到末位（e2,e3,e1），cursor 显示位变化
	m.state.Pending = []PendingEntry{
		m.state.Pending[1], m.state.Pending[2], m.state.Pending[0],
	}
	nm, _ := m.Update(stateChangedMsg{st: m.state, serverUp: true})
	got := nm.(Model)
	if got.mode != modePassword || got.pwTarget != "e1" {
		t.Fatalf("锚定应保持 e1（且 cursor 跟随到新位），got mode=%d target=%q cursor=%d", got.mode, got.pwTarget, got.cursor)
	}
	if got.cursor != 2 {
		t.Fatalf("cursor 应跟随目标到 2, got %d", got.cursor)
	}
}

func TestStateChangedReArmsWatch(t *testing.T) {
	m := testModel(t)
	// 来自 waitForState 的消息（消费了事件）必须补位 re-arm
	nm, cmd := m.Update(stateChangedMsg{st: m.state, serverUp: true, arm: true})
	if cmd == nil {
		t.Fatal("arm=true 的 stateChangedMsg 必须重新武装 waitForState，否则监听断流")
	}
	_ = nm
	// 动作后的主动读（arm=false）不占通道，不得再补位——否则每次动作
	// 净泄漏一个阻塞在 <-ch 的 goroutine。
	nm2, cmd2 := m.Update(stateChangedMsg{st: m.state, serverUp: true})
	if cmd2 != nil {
		t.Fatal("arm=false 的主动读不应 re-arm")
	}
	_ = nm2
}

func TestViewRendersHelpAndEntries(t *testing.T) {
	m := testModel(t)
	v := m.View()
	for _, want := range []string{"j/k 选择", "r 拒绝", "enter 输密码批准", "q 退出", "u1@h1", "cmd2"} {
		if !contains(v, want) {
			t.Fatalf("视图缺少 %q", want)
		}
	}
}

func TestViewForwardHealth(t *testing.T) {
	// 全绿：单行紧凑显示
	m := testModel(t)
	m.state.Forwards = []ForwardView{{Host: "mac-mini", Running: true}}
	v := m.View()
	if !contains(v, "转发:") || !contains(v, "mac-mini") {
		t.Fatalf("转发区缺失: 视图应含 转发: 与 mac-mini")
	}
	// 故障：展开 LastErr 详情行
	m.state.Forwards = append(m.state.Forwards, ForwardView{Host: "nuc", Running: false, Restarts: 4, LastErr: "ssh: connect timeout"})
	v = m.View()
	if !contains(v, "nuc") || !contains(v, "ssh: connect timeout") {
		t.Fatalf("故障主机应展开 LastErr: %s", v)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
