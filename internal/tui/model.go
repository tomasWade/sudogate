package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const maxCmdWidth = 64

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Background(lipgloss.Color("236"))
	downStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	warnStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))
	dimStyle     = lipgloss.NewStyle().Faint(true)
	helpStyle    = lipgloss.NewStyle().Faint(true)
	errStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	okStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))
	promptStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(1, 2)
	confirmStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
)

type mode int

const (
	modeList mode = iota
	modePassword
)

type tickMsg time.Time
type stateChangedMsg struct {
	st        *State
	serverUp  bool
	readDirty bool // state 读到半截 JSON（server 非原子写）：保留旧快照
	arm       bool // true = 来自 waitForState（消费了一个事件，必须补位 re-arm）
}
type actionMsg struct {
	op  string
	id  string
	err error
}

// Model 是 SudoGate 审批 TUI 的顶层模型。只读 state 文件感知变更，
// 动作经 ctl socket 发回 server，二者皆为现有基础设施。
type Model struct {
	statePath string
	ctlPath   string

	state    *State // nil = 文件读不到（server 未运行或从未有请求）
	serverUp bool   // state 缺失时以 ctl socket 探测为准
	cursor   int
	mode     mode
	pwInput  textinput.Model
	pwTarget string    // 密码弹框针对的请求 id；state 滚动后据此取消或对准
	note     string    // 底部状态提示（成功/失败），随 tick 清除
	noteIsEr bool      // note 是否为错误
	noteAt   time.Time // note 写入时刻，3s 后清除
	msgCh    chan struct{}
	width    int
	height   int
}

func New(statePath, ctlPath string) Model {
	pw := textinput.New()
	pw.Placeholder = "sudo 密码"
	pw.EchoMode = textinput.EchoPassword
	pw.EchoCharacter = '•'
	pw.CharLimit = 128
	return Model{
		statePath: statePath,
		ctlPath:   ctlPath,
		msgCh:     make(chan struct{}, 8),
		pwInput:   pw,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(readStateCmd(m.statePath, m.ctlPath), tickCmd(), waitForState(m.statePath, m.ctlPath, m.msgCh))
}

// Run 启动 TUI 程序并在退出时释放 fsnotify 资源。watcher 的生命周期挂在
// Run 而非 Init：bubbletea 在 model 副本上调用 Init，经 model 状态保存的
// stop 函数会随副本丢弃导致 inotify fd 泄漏。watcher 建立失败时降级为
// 2s 轮询心跳，保证 TUI 仍能刷新（只是迟钝）。
func Run(statePath, ctlPath string) error {
	msgCh := make(chan struct{}, 8)
	if stop, err := WatchState(statePath, msgCh); err == nil {
		defer stop()
	} else {
		go func() {
			for range time.Tick(2 * time.Second) {
				select {
				case msgCh <- struct{}{}:
				default:
				}
			}
		}()
	}
	m := New(statePath, ctlPath)
	m.msgCh = msgCh
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// readStateCmd 无条件探测 ctl socket：state 文件可读不代表 server 在跑
// （优雅停机/崩溃都会残留 state），"在线"必须以 ctl 应答为准。读到半截
// JSON（os.WriteFile 非原子写的窗口）标记 readDirty 而非置空，防止密码
// 弹框被瞬时坏读打掉。
func readStateCmd(path, ctlPath string) tea.Cmd {
	return func() tea.Msg {
		// 主动读（初始/动作后兜底）：不占 msgCh 通道，不 re-arm——
		// 已 armed 的 waitForState 等待者继续在岗，避免每动作净增一个
		// 阻塞在 <-ch 的 goroutine。
		return readStateMsg(path, ctlPath, false)
	}
}

func readStateMsg(path, ctlPath string, arm bool) stateChangedMsg {
	msg := stateChangedMsg{serverUp: probeCtl(ctlPath), arm: arm}
	st, err := ReadState(path)
	switch {
	case err == nil:
		msg.st = st
	case errors.Is(err, fs.ErrNotExist):
		// 文件真没了（server 停机清理/从未运行）：st 保持 nil。
	default:
		msg.readDirty = true
	}
	return msg
}

// probeCtl 连一下 ctl socket 判断 server 是否在跑（300ms 内应答）。
func probeCtl(ctlPath string) bool {
	c, err := net.DialTimeout("unix", ctlPath, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// waitForState 阻塞等 fsnotify 事件，到达后立即重读 state 并以
// stateChangedMsg 形式回到 Update 循环。
func waitForState(path, ctlPath string, ch <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-ch
		// 消费了一个事件，必须补位 re-arm，否则监听断流。
		return readStateMsg(path, ctlPath, true)
	}
}

func approveCmd(ctlPath, id, pw string) tea.Cmd {
	return func() tea.Msg {
		return actionMsg{op: "approve", id: id, err: CtlApprove(ctlPath, id, pw)}
	}
}

func denyCmd(ctlPath, id string) tea.Cmd {
	return func() tea.Msg {
		return actionMsg{op: "deny", id: id, err: CtlDeny(ctlPath, id)}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		if m.note != "" && time.Since(m.noteAt) > 3*time.Second {
			m.note = ""
		}
		return m, tickCmd()

	case stateChangedMsg:
		m.serverUp = msg.serverUp
		if !msg.readDirty {
			// 半截 JSON 的瞬时坏读不覆盖旧快照，其余情况正常推进。
			m.state = msg.st
			if m.state != nil {
				// 密码弹框开着时以目标 id 为锚：目标还在就跟随其新位置，
				// 目标没了（过期/被他端处理）则取消输入，防止回车批错对象。
				if m.mode == modePassword {
					idx := indexOfID(m.state.Pending, m.pwTarget)
					if idx < 0 {
						m.mode = modeList
						m.pwTarget = ""
						m.pwInput.Reset()
						m.pwInput.Blur()
						m.note, m.noteIsEr, m.noteAt = "目标请求已失效，密码输入已取消", true, time.Now()
					} else {
						m.cursor = idx
					}
				}
				if m.cursor >= len(m.state.Pending) {
					m.cursor = max(0, len(m.state.Pending)-1)
				}
			}
		}
		// re-arm 仅当消息来自 waitForState（它消费了一个 fsnotify 事件）；
		// 动作后的主动读不占通道，补位反而会泄漏一个额外等待者。
		if msg.arm {
			return m, waitForState(m.statePath, m.ctlPath, m.msgCh)
		}
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.note, m.noteIsEr, m.noteAt = fmt.Sprintf("%s 失败: %v", opName(msg.op), msg.err), true, time.Now()
		} else {
			m.note, m.noteIsEr, m.noteAt = opName(msg.op)+" 完成", false, time.Now()
		}
		// server 落 state 会有事件推送；这里主动重读一次兜底竞态。
		return m, readStateCmd(m.statePath, m.ctlPath)

	case tea.KeyMsg:
		if m.mode == modePassword {
			return m.updatePassword(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

func opName(op string) string {
	if op == "approve" {
		return "批准"
	}
	return "拒绝"
}

func (m Model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := 0
	if m.state != nil {
		n = len(m.state.Pending)
	}
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		if n > 0 {
			m.cursor = (m.cursor + 1) % n
		}
	case "k", "up":
		if n > 0 {
			m.cursor = (m.cursor - 1 + n) % n
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		if n > 0 {
			m.cursor = n - 1
		}
	case "r":
		if n == 0 {
			return m, nil
		}
		id := m.state.Pending[m.cursor].ID
		return m, denyCmd(m.ctlPath, id)
	case "enter":
		if m.state == nil || m.cursor >= len(m.state.Pending) {
			m.mode = modeList
			m.pwInput.Reset()
			m.pwInput.Blur()
			return m, nil
		}
		m.mode = modePassword
		m.pwTarget = m.state.Pending[m.cursor].ID
		m.pwInput.Reset()
		m.pwInput.Focus()
		return m, textinput.Blink
	}
	return m, nil
}

func (m Model) updatePassword(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.pwTarget = ""
		m.pwInput.Reset()
		m.pwInput.Blur()
		return m, nil
	case "enter":
		pw := m.pwInput.Value()
		m.mode = modeList
		m.pwInput.Reset()
		m.pwInput.Blur()
		if pw == "" {
			m.note, m.noteIsEr, m.noteAt = "空密码已取消（输错请用 r 拒绝重试）", true, time.Now()
			return m, nil
		}
		// 以弹框打开时的目标 id 为准，cursor 只是显示位——防止队列滚动后批错对象。
		idx := -1
		if m.state != nil {
			idx = indexOfID(m.state.Pending, m.pwTarget)
		}
		if idx < 0 {
			m.note, m.noteIsEr, m.noteAt = "目标请求已失效，未提交密码", true, time.Now()
			return m, nil
		}
		return m, approveCmd(m.ctlPath, m.pwTarget, pw)
	}
	var cmd tea.Cmd
	m.pwInput, cmd = m.pwInput.Update(msg)
	return m, cmd
}

func indexOfID(list []PendingEntry, id string) int {
	for i := range list {
		if list[i].ID == id {
			return i
		}
	}
	return -1
}

func (m Model) View() string {
	var b strings.Builder

	title := " SudoGate 提权审批 "
	status := errStyle.Render("  ● server 未运行") + dimStyle.Render("  ctl "+m.ctlPath)
	switch {
	case m.state != nil && m.serverUp:
		status = okStyle.Render("  ● 在线 ") + dimStyle.Render(time.Now().Format("15:04:05"))
	case m.state != nil && !m.serverUp:
		// state 可读但 ctl 无应答：server 已停，列表是停机残留。
		status = errStyle.Render("  ● server 已停止") + dimStyle.Render("  （显示为残留状态）")
	case m.state == nil && m.serverUp:
		status = okStyle.Render("  ● 在线 · 空闲") + dimStyle.Render("  （尚无历史请求）")
	}
	b.WriteString(titleStyle.Render(title) + status + "\n\n")

	n := 0
	if m.state != nil {
		n = len(m.state.Pending)
	}
	if m.state == nil {
		if m.serverUp {
			b.WriteString(dimStyle.Render("  server 在线，等待状态文件写入…\n"))
		} else {
			b.WriteString(errStyle.Render("  连不上 ctl socket，请确认 sudogate-server 在跑。\n"))
		}
	} else if n == 0 {
		b.WriteString(dimStyle.Render("  无待批请求。远端 sudo -A 到来时会自动弹出。\n"))
	} else {
		b.WriteString(fmt.Sprintf("  待批 %d 条：\n\n", n))
		for i := range m.state.Pending {
			e := &m.state.Pending[i]
			rem := m.state.RemainingSec(e)
			remStr := fmt.Sprintf("%4ds", max(0, rem))
			remRendered := dimStyle.Render(remStr)
			if rem <= 15 {
				remRendered = downStyle.Render(remStr)
			} else if rem <= 45 {
				remRendered = warnStyle.Render(remStr)
			}
			cmd := e.Command
			if w := lipgloss.Width(cmd); w > maxCmdWidth {
				cmd = truncateCells(cmd, maxCmdWidth) + "…"
			}
			prefix := "  "
			line := fmt.Sprintf("%s@%s  %s  %s", e.User, e.Host, remRendered, cmd)
			if i == m.cursor {
				prefix = cursorStyle.Render(" ▶ ")
				line = lipgloss.NewStyle().Bold(true).Render(line)
			}
			b.WriteString(prefix + line + "\n")
		}
		if sel := &m.state.Pending[m.cursor]; sel.CWD != "" {
			b.WriteString("\n" + dimStyle.Render("  目录: "+sel.CWD) + "\n")
			b.WriteString(dimStyle.Render("  id:   "+sel.ID) + "\n")
		}
	}

	// 转发健康区：全部在线时灰显单行；有故障时逐台展开最近错误。
	if m.state != nil && len(m.state.Forwards) > 0 {
		b.WriteString("\n")
		var parts []string
		broken := false
		for _, f := range m.state.Forwards {
			if f.Running {
				parts = append(parts, okStyle.Render("✓")+f.Host)
			} else {
				broken = true
				parts = append(parts, errStyle.Render("✗")+f.Host)
			}
		}
		b.WriteString(dimStyle.Render("  转发: ") + strings.Join(parts, dimStyle.Render("  ")))
		if broken {
			b.WriteString("\n")
			for _, f := range m.state.Forwards {
				if !f.Running && f.LastErr != "" {
					b.WriteString(errStyle.Render(fmt.Sprintf("  ✗ %s: %s\n", f.Host, truncateCells(f.LastErr, maxCmdWidth))))
				}
			}
		} else {
			b.WriteString("\n")
		}
	}

	if m.mode == modePassword && n > 0 {
		sel := &m.state.Pending[m.cursor]
		box := fmt.Sprintf("批准 %s@%s\n  %s\n\n%s\n\n%s",
			sel.User, sel.Host, sel.Command,
			m.pwInput.View(),
			helpStyle.Render("enter 批准 · esc 取消"))
		b.WriteString("\n" + promptStyle.Render(box) + "\n")
	}

	if m.note != "" {
		style := okStyle
		if m.noteIsEr {
			style = errStyle
		}
		b.WriteString("\n  " + style.Render(m.note) + "\n")
	}

	b.WriteString("\n" + helpStyle.Render("  j/k 选择 · r 拒绝 · enter 输密码批准 · q 退出"))
	return b.String()
}

// truncateCells 按显示宽度（CJK 等宽字符占 2 格）截断，与 lipgloss 的
// 宽度度量一致，避免宽字符命令把列表撑出屏幕。
func truncateCells(s string, maxCells int) string {
	var b strings.Builder
	cells := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if cells+w > maxCells {
			break
		}
		b.WriteRune(r)
		cells += w
	}
	return b.String()
}
