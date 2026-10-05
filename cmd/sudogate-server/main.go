package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"sudogate/internal/proto"
	"sudogate/internal/seal"
	"sudogate/internal/server"
)

//go:embed laptop.key
var keyPEM []byte

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "review":
		fs := flag.NewFlagSet("review", flag.ExitOnError)
		socket := fs.String("socket", "", "数据 socket 路径")
		id := fs.String("id", "", "指定请求 id（默认最旧一条）")
		fs.Parse(os.Args[2:])
		os.Exit(server.RunReview(ctlPath(*socket), *id))
	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		socket := fs.String("socket", "", "数据 socket 路径")
		waybar := fs.Bool("waybar", false, "输出 waybar JSON")
		fs.Parse(os.Args[2:])
		os.Exit(server.RunStatus(ctlPath(*socket), *waybar))
	case "approve":
		fs := flag.NewFlagSet("approve", flag.ExitOnError)
		socket := fs.String("socket", "", "数据 socket 路径")
		id := fs.String("id", "", "请求 id")
		fs.Parse(os.Args[2:])
		if *id == "" {
			fmt.Fprintln(os.Stderr, "approve 需要 -id")
			os.Exit(2)
		}
		os.Exit(server.RunApprove(ctlPath(*socket), *id))
	case "deny":
		fs := flag.NewFlagSet("deny", flag.ExitOnError)
		socket := fs.String("socket", "", "数据 socket 路径")
		id := fs.String("id", "", "请求 id")
		fs.Parse(os.Args[2:])
		if *id == "" {
			fmt.Fprintln(os.Stderr, "deny 需要 -id")
			os.Exit(2)
		}
		os.Exit(server.RunDeny(ctlPath(*socket), *id))
	default:
		usage()
		os.Exit(2)
	}
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := fs.String("socket", "", "数据 socket 路径")
	timeout := fs.Int("timeout", 120, "每请求等待秒数")
	maxPending := fs.Int("max-pending", 5, "待批上限")
	fs.Parse(args)
	priv, err := seal.LoadPrivateKey(keyPEM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate-server: 内嵌私钥无效:", err)
		os.Exit(1)
	}
	dataPath := *socket
	if dataPath == "" {
		dataPath = proto.DefaultSocket()
	}
	statePath := statePath()
	auditPath := auditPath()
	if err := server.New(priv, server.Options{
		SocketPath: dataPath,
		Timeout:    time.Duration(*timeout) * time.Second,
		MaxPending: *maxPending,
		StatePath:  statePath,
		AuditPath:  auditPath,
	}).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "sudogate-server:", err)
		os.Exit(1)
	}
}

func ctlPath(socket string) string {
	if socket == "" {
		socket = proto.DefaultSocket()
	}
	return server.CtlPath(socket)
}

func statePath() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d + "/sudogate.state"
	}
	return filepath.Join(os.TempDir(), "sudogate-"+userName()+".state")
}

func auditPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "sudogate", "audit.jsonl")
}

func userName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "user"
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法: sudogate-server <子命令>
  serve   [-socket 路径] [-timeout 秒] [-max-pending N]   常驻服务
  review  [-socket 路径] [-id ID]                          审阅最旧/指定请求并输密码
  status  [-socket 路径] [--waybar]                        查看待批
  approve -id ID [-socket 路径]                            从 stdin 读密码批准（测试用）
  deny    -id ID [-socket 路径]                            拒绝`)
}
