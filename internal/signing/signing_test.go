package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSignVerifyAndKeyRotation(t *testing.T) {
	tmp := t.TempDir()
	keysDir := filepath.Join(tmp, "keys.d")
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		t.Fatal(err)
	}

	pub1, priv1, id1, err := GenerateKeyPair("old-key")
	if err != nil {
		t.Fatalf("GenerateKeyPair(1): %v", err)
	}
	pub2, priv2, id2, err := GenerateKeyPair("new-key")
	if err != nil {
		t.Fatalf("GenerateKeyPair(2): %v", err)
	}

	if err := os.WriteFile(filepath.Join(keysDir, "old.pub"), pub1, 0o644); err != nil {
		t.Fatal(err)
	}
	priv1Path := filepath.Join(tmp, "old.key")
	priv2Path := filepath.Join(tmp, "new.key")
	_ = os.WriteFile(priv1Path, priv1, 0o600)
	_ = os.WriteFile(priv2Path, priv2, 0o600)

	target := filepath.Join(tmp, "index.json")
	sigPath := target + ".sig"
	_ = os.WriteFile(target, []byte(`{"schema_version":1,"packages":{}}`), 0o644)

	// Sign with old key -> verifies
	gotID, err := SignFile(priv1Path, target, sigPath)
	if err != nil || gotID != id1 {
		t.Fatalf("SignFile(1): id=%q err=%v", gotID, err)
	}
	matched, err := VerifyFile(keysDir, target, sigPath)
	if err != nil || matched.KeyID != id1 {
		t.Fatalf("VerifyFile(1): matched=%+v err=%v", matched, err)
	}

	// Tamper payload -> must fail
	_ = os.WriteFile(target, []byte(`{"schema_version":1,"packages":{"evil":{}}}`), 0o644)
	if _, err := VerifyFile(keysDir, target, sigPath); err == nil {
		t.Fatal("expected verification failure on tampered payload")
	}
	_ = os.WriteFile(target, []byte(`{"schema_version":1,"packages":{}}`), 0o644)

	// Repo moved & signed with new key before user trusts it -> fails with clear rotation hint
	if _, err := SignFile(priv2Path, target, sigPath); err != nil {
		t.Fatalf("SignFile(2): %v", err)
	}
	if _, err := VerifyFile(keysDir, target, sigPath); err == nil {
		t.Fatal("expected untrusted new key to fail before trusting")
	}

	// User trusts new key in keys.d -> verifies seamlessly alongside old key!
	_ = os.WriteFile(filepath.Join(keysDir, "new.pub"), pub2, 0o644)
	matched2, err := VerifyFile(keysDir, target, sigPath)
	if err != nil || matched2.KeyID != id2 {
		t.Fatalf("VerifyFile(2) after rotation: matched=%+v err=%v", matched2, err)
	}
}

func TestParseOpenSSHEd25519PublicKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Construct RFC 4253 ssh-ed25519 wire blob
	var blob []byte
	typeStr := "ssh-ed25519"
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(typeStr)))
	blob = append(blob, typeStr...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(pub)))
	blob = append(blob, pub...)

	sshLine := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " release@kryx-linux\n"
	pk, err := ParsePublicKey([]byte(sshLine))
	if err != nil {
		t.Fatalf("ParsePublicKey(ssh-ed25519): %v", err)
	}
	if pk.KeyID != KeyIDOf(pub) || pk.Comment != "release@kryx-linux" {
		t.Fatalf("unexpected parsed ssh key: %+v", pk)
	}

	payload := []byte("hello kryx")
	sigData, _ := SignBytes(priv, payload)
	if _, err := VerifyBytes([]PublicKey{pk}, payload, sigData); err != nil {
		t.Fatalf("VerifyBytes with ssh-ed25519 pubkey failed: %v", err)
	}
}
