package seal

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"

	"sudogate/internal/proto"

	"golang.org/x/crypto/chacha20poly1305"
)

func LoadPrivateKey(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 private key")
	}
	return priv, nil
}

func LoadPublicKey(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 public key")
	}
	return pub, nil
}

func Fingerprint(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:])[:8]
}

func deriveKey(secret []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, []byte(proto.Label), "seal", chacha20poly1305.KeySize)
}

func GenerateEphemeral() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

func SealPassword(peerPub []byte, id []byte, command string, password string) (*proto.Seal, error) {
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, err
	}
	eph, err := GenerateEphemeral()
	if err != nil {
		return nil, err
	}
	secret, err := eph.ECDH(pub)
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(secret)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, proto.Plaintext(password, id), proto.AAD(id, command))
	return &proto.Seal{
		EPub:  base64.StdEncoding.EncodeToString(eph.PublicKey().Bytes()),
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	}, nil
}

func Open(priv *ecdh.PrivateKey, s *proto.Seal, id []byte, command string) (string, error) {
	ephPub, err := base64.StdEncoding.DecodeString(s.EPub)
	if err != nil {
		return "", err
	}
	pub, err := ecdh.X25519().NewPublicKey(ephPub)
	if err != nil {
		return "", err
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return "", err
	}
	key, err := deriveKey(secret)
	if err != nil {
		return "", err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(s.Nonce)
	if err != nil || len(nonce) != aead.NonceSize() {
		return "", errors.New("bad nonce")
	}
	ct, err := base64.StdEncoding.DecodeString(s.CT)
	if err != nil {
		return "", err
	}
	pt, err := aead.Open(nil, nonce, ct, proto.AAD(id, command))
	if err != nil {
		return "", err
	}
	return proto.SplitPlaintext(pt, id)
}

func Sign(priv ed25519.PrivateKey, id []byte, command string, peerPub []byte) []byte {
	return ed25519.Sign(priv, proto.SignPayload(id, command, peerPub))
}

func Verify(pub ed25519.PublicKey, id []byte, command string, peerPub []byte, sig []byte) bool {
	return ed25519.Verify(pub, proto.SignPayload(id, command, peerPub), sig)
}
