package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"sudogate/internal/server"
)

type PendingEntry struct {
	ID      string `json:"id"`
	Host    string `json:"host"`
	User    string `json:"user"`
	Command string `json:"command"`
	CWD     string `json:"cwd"`
	AgeSec  int64  `json:"age_sec"`
}

// ForwardView 是 state 文件中转发健康快照的镜像（磁盘契约，
// 与 server.ForwardState 的 json 字段保持一致）。
type ForwardView struct {
	Host       string `json:"host"`
	RemotePath string `json:"remote_path,omitempty"`
	Running    bool   `json:"running"`
	Restarts   int    `json:"restarts"`
	LastStart  string `json:"last_start,omitempty"`
	LastErr    string `json:"last_err,omitempty"`
}

type State struct {
	Updated    time.Time      `json:"updated"`
	TimeoutSec int64          `json:"timeout_sec"`
	Forwards   []ForwardView  `json:"forwards,omitempty"`
	Pending    []PendingEntry `json:"pending"`
}

// RemainingSec 返回某条请求按本端时钟折算后的剩余等待秒数（可为负）。
func (s *State) RemainingSec(e *PendingEntry) int64 {
	if s == nil || s.TimeoutSec <= 0 {
		return 0
	}
	drift := int64(time.Since(s.Updated) / time.Second)
	return s.TimeoutSec - e.AgeSec - drift
}

// DefaultStatePath 与 sudogate-server 写状态的路径保持一致。
func DefaultStatePath() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "sudogate.state")
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "user"
	}
	return filepath.Join(os.TempDir(), "sudogate-"+user+".state")
}

func ReadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state 文件损坏: %w", err)
	}
	if st.TimeoutSec <= 0 {
		st.TimeoutSec = 120
	}
	if st.Pending == nil {
		st.Pending = []PendingEntry{}
	}
	if st.Forwards == nil {
		st.Forwards = []ForwardView{}
	}
	return &st, nil
}

// WatchState 监听 state 文件所在目录（免疫原地覆盖写与 tmp+rename 两种
// 落盘方式），文件名匹配且有写入类事件时经 100ms 防抖后向 out 投递。
// fsnotify 的 Errors 也被消费并当作一次投递：v1.10.x 中 sendError 与
// 事件泵共用 goroutine，不排空的话一个 EventOverflow 就会永久卡死
// 事件流（XDG_RUNTIME_DIR 是繁忙目录，溢出并不罕见）；溢出意味着
// 丢事件，触发一次重读正好兜底。返回的 stop 用于释放 inotify 资源。
func WatchState(path string, out chan<- struct{}) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, err
	}
	go func() {
		var timer *time.Timer
		fire := func() {
			select {
			case out <- struct{}{}:
			default:
			}
		}
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) != filepath.Clean(path) || ev.Has(fsnotify.Chmod) {
					continue
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
				// 错误（典型如事件溢出）→ 重读兜底
			}
			if timer == nil {
				timer = time.AfterFunc(100*time.Millisecond, fire)
			} else {
				timer.Reset(100 * time.Millisecond)
			}
		}
	}()
	return func() { w.Close() }, nil
}

// CtlApprove 经 ctl socket 批准某请求（与 CLI/插件同一后端）。
func CtlApprove(ctlPath, id, password string) error {
	r, err := server.CtlCall(ctlPath, &server.CtlRequest{Op: "approve", ID: id, Password: password})
	if err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}

// CtlDeny 经 ctl socket 拒绝某请求。
func CtlDeny(ctlPath, id string) error {
	r, err := server.CtlCall(ctlPath, &server.CtlRequest{Op: "deny", ID: id})
	if err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}
