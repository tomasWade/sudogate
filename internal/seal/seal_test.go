package seal

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"sudogate/internal/proto"
)

func testID() []byte { return []byte("0123456789abcdef") }

func TestSealRoundtrip(t *testing.T) {
	eph, err := GenerateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	s, err := SealPassword(eph.PublicKey().Bytes(), testID(), "apt install -y jq", "s3cret-pw")
	if err != nil {
		t.Fatal(err)
	}
	pw, err := Open(eph, s, testID(), "apt install -y jq")
	if err != nil {
		t.Fatal(err)
	}
	if pw != "s3cret-pw" {
		t.Fatalf("password mismatch: %q", pw)
	}
}

func TestSealWrongID(t *testing.T) {
	eph, _ := GenerateEphemeral()
	s, _ := SealPassword(eph.PublicKey().Bytes(), testID(), "cmd", "pw")
	if _, err := Open(eph, s, []byte("ffffffffffffffff"), "cmd"); err == nil {
		t.Fatal("expected failure with wrong id")
	}
}

func TestSealWrongCommand(t *testing.T) {
	eph, _ := GenerateEphemeral()
	s, _ := SealPassword(eph.PublicKey().Bytes(), testID(), "cmd-a", "pw")
	if _, err := Open(eph, s, testID(), "cmd-b"); err == nil {
		t.Fatal("expected failure with wrong command")
	}
}

func TestSealTamperedCT(t *testing.T) {
	eph, _ := GenerateEphemeral()
	s, _ := SealPassword(eph.PublicKey().Bytes(), testID(), "cmd", "pw")
	ct, _ := base64.StdEncoding.DecodeString(s.CT)
	ct[0] ^= 0x01
	s.CT = base64.StdEncoding.EncodeToString(ct)
	if _, err := Open(eph, s, testID(), "cmd"); err == nil {
		t.Fatal("expected failure with tampered ciphertext")
	}
}

func TestSealWrongPeer(t *testing.T) {
	eph, _ := GenerateEphemeral()
	other, _ := GenerateEphemeral()
	s, _ := SealPassword(eph.PublicKey().Bytes(), testID(), "cmd", "pw")
	if _, err := Open(other, s, testID(), "cmd"); err == nil {
		t.Fatal("expected failure with wrong peer key")
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	eph, _ := GenerateEphemeral()
	epub := eph.PublicKey().Bytes()
	sig := Sign(priv, testID(), "cmd", epub)
	if !Verify(pub, testID(), "cmd", epub, sig) {
		t.Fatal("valid signature rejected")
	}
	if Verify(pub, testID(), "other-cmd", epub, sig) {
		t.Fatal("signature accepted for wrong command")
	}
	sig[0] ^= 0x01
	if Verify(pub, testID(), "cmd", epub, sig) {
		t.Fatal("tampered signature accepted")
	}
}

func TestPlaintextBinding(t *testing.T) {
	pt := proto.Plaintext("pw", testID())
	pw, err := proto.SplitPlaintext(pt, testID())
	if err != nil || pw != "pw" {
		t.Fatalf("roundtrip failed: %v %q", err, pw)
	}
	if _, err := proto.SplitPlaintext(pt, []byte("ffffffffffffffff")); err == nil {
		t.Fatal("expected id mismatch failure")
	}
}
