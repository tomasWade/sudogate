package server

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

func RunReview(ctlPath, idArg string) int {
	reply, err := CtlCall(ctlPath, &CtlRequest{Op: "list"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 无法连接控制接口（server 未运行？）:", err)
		return 1
	}
	if len(reply.Pending) == 0 {
		fmt.Println("无待批请求")
		return 0
	}
	var target *EntryInfo
	for i := range reply.Pending {
		e := &reply.Pending[i]
		if idArg != "" && e.ID == idArg {
			target = e
			break
		}
	}
	if target == nil {
		target = &reply.Pending[0]
	}
	fmt.Printf("════════ SudoGate 提权请求 ════════\n")
	fmt.Printf("  主机:  %s\n", target.Host)
	fmt.Printf("  用户:  %s\n", target.User)
	fmt.Printf("  目录:  %s\n", target.CWD)
	fmt.Printf("  命令:  %s\n", target.Command)
	fmt.Printf("  已等待: %ds\n", target.AgeSec)
	if len(reply.Pending) > 1 {
		fmt.Printf("  （另有 %d 条待批）\n", len(reply.Pending)-1)
	}
	fmt.Printf("──────────────────────────────────\n")
	fmt.Printf("  只批准与 opencode 闸门1 刚批过、命令对得上的请求\n")
	fmt.Printf("密码（直接回车 = 拒绝）: ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 读取密码失败:", err)
		return 1
	}
	password := strings.TrimRight(string(pw), "\r\n")
	var op string
	if password == "" {
		op = "deny"
	} else {
		op = "approve"
	}
	r, err := CtlCall(ctlPath, &CtlRequest{Op: op, ID: target.ID, Password: password})
	if err != nil || !r.OK {
		fmt.Fprintf(os.Stderr, "sudogate: %s 失败: %v\n", op, errOr(r, err))
		return 1
	}
	if op == "approve" {
		fmt.Println("已批准，密码已送达")
	} else {
		fmt.Println("已拒绝")
	}
	return 0
}

func errOr(r *CtlReply, err error) string {
	if err != nil {
		return err.Error()
	}
	return r.Error
}

func RunApprove(ctlPath, id string) int {
	pw, err := readLine(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 从 stdin 读密码失败:", err)
		return 1
	}
	r, err := CtlCall(ctlPath, &CtlRequest{Op: "approve", ID: id, Password: pw})
	if err != nil || !r.OK {
		fmt.Fprintf(os.Stderr, "sudogate: approve 失败: %v\n", errOr(r, err))
		return 1
	}
	fmt.Println("已批准，密码已送达")
	return 0
}

func RunDeny(ctlPath, id string) int {
	r, err := CtlCall(ctlPath, &CtlRequest{Op: "deny", ID: id})
	if err != nil || !r.OK {
		fmt.Fprintf(os.Stderr, "sudogate: deny 失败: %v\n", errOr(r, err))
		return 1
	}
	fmt.Println("已拒绝")
	return 0
}

func RunStatus(ctlPath string, waybar bool) int {
	reply, err := CtlCall(ctlPath, &CtlRequest{Op: "list"})
	if err != nil {
		if waybar {
			fmt.Println(`{"text":"","class":"","tooltip":"sudogate server 未运行"}`)
			return 0
		}
		fmt.Fprintln(os.Stderr, "sudogate: server 未运行:", err)
		return 1
	}
	if !waybar {
		if len(reply.Pending) == 0 {
			fmt.Println("无待批请求")
			return 0
		}
		for _, e := range reply.Pending {
			cmd := e.Command
			if len(cmd) > 72 {
				cmd = cmd[:72] + "…"
			}
			fmt.Printf("%s  %s@%s  %2ds  %s\n", e.ID, e.User, e.Host, e.AgeSec, cmd)
		}
		return 0
	}
	n := len(reply.Pending)
	if n == 0 {
		fmt.Println(`{"text":"","class":"","tooltip":"sudo 待批 0"}`)
		return 0
	}
	var lines []string
	for _, e := range reply.Pending {
		cmd := e.Command
		if len(cmd) > 60 {
			cmd = cmd[:60] + "…"
		}
		lines = append(lines, fmt.Sprintf("%s@%s: %s", e.User, e.Host, cmd))
	}
	tooltip := strings.ReplaceAll(strings.Join(lines, "\n"), `"`, `\"`)
	fmt.Printf(`{"text":"sudo %d","class":"pending","tooltip":"%s"}`+"\n", n, tooltip)
	return 0
}

func readLine(f *os.File) (string, error) {
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
