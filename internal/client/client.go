package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sudogate/internal/proto"
	"sudogate/internal/seal"
)

const DefaultTimeout = 120 * time.Second

var flagWithArg = map[string]bool{
	"-C": true, "-D": true, "-h": true, "-p": true,
	"-u": true, "-U": true, "-g": true, "-T": true,
}

func ParentCommand(ppid int) string {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(ppid)).Output()
	if err != nil {
		return "Unknown command"
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "Unknown command"
	}
	if filepath.Base(fields[0]) != "sudo" {
		return strings.Join(fields, " ")
	}
	i := 1
	for i < len(fields) {
		f := fields[i]
		if f == "--" {
			i++
			break
		}
		if strings.HasPrefix(f, "-") && len(f) > 1 {
			if flagWithArg[f] {
				i += 2
			} else {
				i++
			}
			continue
		}
		break
	}
	if i >= len(fields) {
		return strings.Join(fields, " ")
	}
	return strings.Join(fields[i:], " ")
}

func RunAskpass(pub ed25519.PublicKey) int {
	return run(pub, false)
}

func RunSelfTest(pub ed25519.PublicKey) int {
	return run(pub, true)
}

func run(pub ed25519.PublicKey, selfTest bool) int {
	socket := proto.DefaultSocket()
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		fmt.Fprintln(os.Stderr, "sudogate:", err)
		return 1
	}
	eph, err := seal.GenerateEphemeral()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate:", err)
		return 1
	}
	ppid := os.Getppid()
	command := ParentCommand(ppid)
	if selfTest {
		command = "sudogate-client self-test"
	}
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	cwd, _ := os.Getwd()
	req := &proto.Request{
		V:       proto.Version,
		ID:      hex.EncodeToString(id),
		Host:    host,
		User:    user,
		Command: command,
		CWD:     cwd,
		PPID:    ppid,
		TS:      time.Now().Unix(),
		EPub:    base64.StdEncoding.EncodeToString(eph.PublicKey().Bytes()),
	}
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sudogate: 无法连接 %s（ssh -R 未连接或 server 未运行？）: %v\n", socket, err)
		return 1
	}
	defer conn.Close()
	if err := proto.WriteFrame(conn, req); err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 发送请求失败:", err)
		return 1
	}
	if err := conn.SetReadDeadline(time.Now().Add(DefaultTimeout)); err != nil {
		fmt.Fprintln(os.Stderr, "sudogate:", err)
		return 1
	}
	var resp proto.Response
	if err := proto.ReadFrame(conn, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 读取响应失败:", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "sudogate: 请求被拒绝（%s）\n", resp.Reason)
		return 1
	}
	sig, err := base64.StdEncoding.DecodeString(resp.Sig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 签名编码错误:", err)
		return 1
	}
	epub := eph.PublicKey().Bytes()
	if !seal.Verify(pub, id, command, epub, sig) {
		fmt.Fprintf(os.Stderr, "sudogate: 验签失败（本二进制信任公钥指纹 %s），可能混编舰队或响应被篡改\n", seal.Fingerprint(pub))
		return 1
	}
	password, err := seal.Open(eph, resp.Seal, id, command)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 解封失败:", err)
		return 1
	}
	if selfTest {
		sum := sha256.Sum256([]byte(password))
		fmt.Printf("self-test OK: 收到 %d 字节, sha256=%s, 信任公钥指纹=%s\n",
			len(password), hex.EncodeToString(sum[:])[:12], seal.Fingerprint(pub))
		return 0
	}
	os.Stdout.WriteString(password + "\n")
	return 0
}

func Prune() int {
	socket := proto.DefaultSocket()
	if _, err := os.Stat(socket); os.IsNotExist(err) {
		fmt.Println("无 socket 文件:", socket)
		return 0
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err == nil {
		conn.Close()
		fmt.Println("socket 活跃，无需清理:", socket)
		return 0
	}
	if err := os.Remove(socket); err != nil {
		fmt.Fprintln(os.Stderr, "sudogate: 清理失败:", err)
		return 1
	}
	fmt.Println("已清理残留 socket:", socket)
	return 0
}
