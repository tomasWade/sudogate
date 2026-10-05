package main

import (
	_ "embed"
	"fmt"
	"os"

	"sudogate/internal/client"
	"sudogate/internal/seal"
)

//go:embed laptop.pub
var pubPEM []byte

func main() {
	pub, err := seal.LoadPublicKey(pubPEM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate-client: 内嵌公钥无效:", err)
		os.Exit(1)
	}
	// sudo execs the SUDO_ASKPASS program with the prompt string as argv[1]
	// (and bare, with no argv at all, on some builds) — so anything that is
	// not a known subcommand IS the askpass run. Explicit "askpass" kept for
	// humans/scripts; usage only via -h/--help.
	if len(os.Args) < 2 {
		os.Exit(client.RunAskpass(pub))
	}
	switch os.Args[1] {
	case "test":
		os.Exit(client.RunSelfTest(pub))
	case "prune":
		os.Exit(client.Prune())
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		os.Exit(client.RunAskpass(pub))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法: sudogate-client [子命令]
  (无参数)     作为 SUDO_ASKPASS helper 运行——sudo 调用即此模式（prompt 参数被忽略）
  askpass     同上（显式，或任何非子命令参数）
  test        自检：构造请求走完整链路，打印摘要（不含密码明文）
  prune       清理残留 socket 文件`)
}
