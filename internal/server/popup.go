package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// PopupManager：请求到达时在桌面弹出审批终端窗口（kitty 等）。
//
// 机制：addPending 后队列非空且弹窗进程不在 → 读 popup.conf 的命令
// 模板、把 {tui} 占位符替换为 sudogate-tui --until-empty 并 spawn。
// 模板完全外置（配置即终端：换 kitty/foot/alacritty 只改配置行，
// server 零终端知识），server 与终端之间唯一契约是 {tui} token。
//
// 与 ForwardManager 同构的生命周期纪律：
//   - Notify 必须在 server.mu 之外调用（本 manager 有自己的锁）
//   - 弹窗子进程不设 Pdeathsig 且独立会话（Setsid），server 停止/
//     重启不会用信号强杀它——窗口总是经 TUI 自身优雅退出（队列清空
//     或 server 停机清队后 state 变空，--until-empty 自动收窗）
type PopupManager struct {
	mu       sync.Mutex
	confPath string
	tuiPath  string // 替换 {tui} 的目标（绝对路径，测试可注入假脚本）
	cmd      *exec.Cmd
}

// NewPopupManager 构造；tuiPath 为空时自动探测（PATH → ~/.local/bin）。
func NewPopupManager(confPath, tuiPath string) *PopupManager {
	if tuiPath == "" {
		tuiPath = locateTUI()
	}
	return &PopupManager{confPath: confPath, tuiPath: tuiPath}
}

// locateTUI 探测 sudogate-tui：server 跑在 systemd user service 里时
// PATH 往往不含 ~/.local/bin，两级探测 + 兜底裸名（交给用户 shell 环境）。
func locateTUI() string {
	if p, err := exec.LookPath("sudogate-tui"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "sudogate-tui")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "sudogate-tui"
}

// template 每次触发时重读 conf：编辑即生效（换终端/改尺寸无需重启
// server），文件不存在或无有效行 = 功能关闭。# 开头的注释行与空行
// 剥离（popup.conf.example 里全是注释示例，直接复制不能让注释变 argv）。
func (p *PopupManager) template() string {
	b, err := os.ReadFile(p.confPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// Notify 在每条 addPending 成功后调用。pendingLen 为当前队列长度；
// 弹窗活着时静默（TUI 经 fsnotify 自己长行），死了且队列非空才弹。
func (p *PopupManager) Notify(pendingLen int) {
	if pendingLen == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tpl := p.template()
	if tpl == "" {
		return
	}
	if p.cmd != nil {
		return // 弹窗还活着（Wait 回调负责置 nil）
	}
	// tuiPath 为空（未注入）则每次 spawn 时现解析：server 跑在
	// systemd service 里 PATH 常缺 ~/.local/bin，若 TUI 在 server
	// 启动后才安装（make install-tui），启动期解析一次会一直失败。
	if p.tuiPath == "" {
		p.tuiPath = locateTUI()
	}
	args, err := expandTemplate(tpl, p.tuiPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sudogate popup: %v\n", err)
		return
	}
	c := exec.Command(args[0], args[1:]...)
	// 新会话：弹窗独立成组，server 退出/重启不牵连（无 Pdeathsig，
	// 有 setsid）；GUI 所需的 WAYLAND_DISPLAY 等继承自 server 环境。
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "sudogate popup: spawn %s: %v\n", args[0], err)
		return
	}
	p.cmd = c
	go func() {
		c.Wait()
		p.mu.Lock()
		if p.cmd == c {
			p.cmd = nil
		}
		p.mu.Unlock()
	}()
}

// expandTemplate：按空白切分模板，独立 token "{tui}" 展开为
// "<tuiPath> --until-empty" 两个参数。模板不支持引号/含空格参数
// （终端命令行都不需要，README 有说明）；缺失 {tui} 也拒绝——
// 否则终端裸跑（窗口不随队列清空自灭，且压着 p.cmd 停掉后续重弹，
// 功能静默失效），每次触发的报错日志比哑故障好查得多。
func expandTemplate(tpl, tuiPath string) ([]string, error) {
	var args []string
	sawTUI := false
	for _, tok := range strings.Fields(tpl) {
		switch {
		case tok == "{tui}":
			args = append(args, tuiPath, "--until-empty")
			sawTUI = true
		case strings.Contains(tok, "{tui}"):
			return nil, fmt.Errorf("模板 token %q 内嵌 {tui}（必须独立成词）", tok)
		default:
			args = append(args, tok)
		}
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("模板为空")
	}
	if !sawTUI {
		return nil, fmt.Errorf("模板缺少 {tui} 占位符")
	}
	return args, nil
}

// Status 供 ctl 汇报。
func (p *PopupManager) Status() (enabled bool, tpl string, windowOpen bool) {
	tpl = p.template()
	p.mu.Lock()
	defer p.mu.Unlock()
	return tpl != "", tpl, p.cmd != nil
}

// PopupOn 写入默认模板（已存在则保持用户内容），PopupOff 删除 conf。
func PopupOn(confPath, defaultTpl string) error {
	if _, err := os.Stat(confPath); err == nil {
		return nil // 已开启（或用户已自定义），幂等
	}
	os.MkdirAll(filepath.Dir(confPath), 0o700)
	return os.WriteFile(confPath, []byte(defaultTpl+"\n"), 0o600)
}

func PopupOff(confPath string) error {
	err := os.Remove(confPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// DefaultPopupConf 返回默认弹窗配置路径（与 forward.conf 同目录）。
func DefaultPopupConf() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "sudogate", "popup.conf")
}

// DefaultPopupTemplate 默认 kitty 模板（按字符格定尺寸，不依赖 WM；
// 浮动/居中由 Hyprland windowrule 按 app-id 匹配，见 README）。
const DefaultPopupTemplate = `kitty --app-id sudogate-approve --override initial_window_width=90c --override initial_window_height=26c --override remember_window_size=no -e {tui}`

// RunPopup 实现 `sudogate-server popup on|off|status`。
func RunPopup(ctlPath string, args []string) int {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "on", "off", "status":
	default:
		fmt.Fprintln(os.Stderr, "用法: sudogate-server popup [-socket 路径] on|off|status")
		return 2
	}
	r, err := CtlCall(ctlPath, &CtlRequest{Op: "popup", Action: action})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: server 未运行:", err)
		return 3
	}
	if !r.OK {
		fmt.Fprintln(os.Stderr, "sudogate: popup 失败:", r.Error)
		return 1
	}
	state := "off"
	if r.PopupOn {
		state = "on"
	}
	window := "弹窗未开"
	if r.PopupOpen {
		window = "弹窗开着"
	}
	fmt.Printf("popup: %s（%s）\n", state, window)
	if r.PopupTpl != "" {
		fmt.Println("模板:", r.PopupTpl)
		fmt.Println("编辑该文件可换终端/尺寸（{tui} 占位符须独立成词），即时生效")
	}
	return 0
}
