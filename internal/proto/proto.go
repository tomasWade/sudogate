package proto

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const Version = 1
const Label = "sudogate-v1"
const MaxCommandLen = 4096
const MaxFrameLen = 64 * 1024

const ReasonDenied = "denied"
const ReasonTimeout = "timeout"
const ReasonBusy = "busy"
const ReasonMalformed = "malformed"

type Request struct {
	V       int    `json:"v"`
	ID      string `json:"id"`
	Host    string `json:"host"`
	User    string `json:"user"`
	Command string `json:"command"`
	CWD     string `json:"cwd"`
	PPID    int    `json:"ppid"`
	TS      int64  `json:"ts"`
	EPub    string `json:"epub"`
}

type Seal struct {
	EPub  string `json:"eph_pub"`
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

type Response struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	ID     string `json:"id"`
	Seal   *Seal  `json:"seal,omitempty"`
	Sig    string `json:"sig,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func SignPayload(id []byte, command string, epub []byte) []byte {
	ch := sha256.Sum256([]byte(command))
	eh := sha256.Sum256(epub)
	buf := make([]byte, 0, len(Label)+len(id)+len(ch)+len(eh))
	buf = append(buf, Label...)
	buf = append(buf, id...)
	buf = append(buf, ch[:]...)
	buf = append(buf, eh[:]...)
	return buf
}

func AAD(id []byte, command string) []byte {
	ch := sha256.Sum256([]byte(command))
	return append(append([]byte{}, id...), ch[:]...)
}

func Plaintext(password string, id []byte) []byte {
	pt := make([]byte, 0, len(password)+1+len(id))
	pt = append(pt, password...)
	pt = append(pt, 0x00)
	return append(pt, id...)
}

func SplitPlaintext(pt []byte, id []byte) (string, error) {
	if len(pt) <= 1+len(id) {
		return "", errors.New("plaintext too short")
	}
	cut := len(pt) - 1 - len(id)
	if pt[cut] != 0x00 {
		return "", errors.New("plaintext separator missing")
	}
	if !strings.EqualFold(hex.EncodeToString(pt[cut+1:]), hex.EncodeToString(id)) {
		return "", errors.New("plaintext id mismatch")
	}
	return string(pt[:cut]), nil
}

func ValidateRequest(r *Request, now int64) error {
	if r.V != Version {
		return fmt.Errorf("unsupported version %d", r.V)
	}
	raw, err := hex.DecodeString(r.ID)
	if err != nil || len(raw) != 16 {
		return errors.New("id must be 16 bytes hex")
	}
	if r.Command == "" || len(r.Command) > MaxCommandLen {
		return errors.New("command empty or too long")
	}
	if len(r.Host) == 0 || len(r.Host) > 255 {
		return errors.New("host empty or too long")
	}
	if len(r.User) == 0 || len(r.User) > 255 {
		return errors.New("user empty or too long")
	}
	if d := now - r.TS; d < -60 || d > 60 {
		return fmt.Errorf("timestamp skew %ds", d)
	}
	if b, err := b64Decode(r.EPub); err != nil || len(b) != 32 {
		return errors.New("epub must be 32 bytes base64")
	}
	return nil
}

func b64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func ReadFrame(r io.Reader, v any) error {
	br := bufio.NewReaderSize(r, MaxFrameLen)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return err
	}
	if len(line) > MaxFrameLen {
		return errors.New("frame too long")
	}
	return json.Unmarshal(line[:len(line)-1], v)
}

func DefaultSocket() string {
	if p := os.Getenv("SUDOGATE_SOCK"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d + "/sudogate.sock"
	}
	return "/tmp/sudogate-" + os.Getenv("USER") + ".sock"
}
