package main

import (
	"flag"
	"fmt"
	"os"

	"sudogate/internal/proto"
	"sudogate/internal/server"
	"sudogate/internal/tui"
)

func main() {
	fs := flag.NewFlagSet("sudogate-tui", flag.ExitOnError)
	socket := fs.String("socket", "", "server 数据 socket 路径（ctl 路径由此派生）")
	untilEmpty := fs.Bool("until-empty", false, "临时模式：队列空即自动退出（herdr 插件 tab 等宿主场景；启动即空同样退出）")
	fs.Parse(os.Args[1:])

	dataPath := *socket
	if dataPath == "" {
		dataPath = proto.DefaultSocket()
	}

	if err := tui.Run(tui.DefaultStatePath(), server.CtlPath(dataPath), *untilEmpty); err != nil {
		fmt.Fprintln(os.Stderr, "sudogate-tui:", err)
		os.Exit(1)
	}
}
