// Package signing implements pure-Go Ed25519 release signing and keyring
// verification (supporting both native kryx-ed25519 keys and OpenSSH ssh-ed25519
// public keys) with multi-key rotation and recovery support.
package signing

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	PubKeyHeader  = "kryx-ed25519-pub-v1"
	PrivKeyHeader = "kryx-ed25519-key-v1"
	SigHeader     = "kryx-ed25519-sig-v1"
)

// PublicKey represents a trusted Ed25519 verification key loaded from keys.d.
type PublicKey struct {
	Name        string
	KeyID       string // First 16 hex chars of SHA-256(raw 32-byte pubkey)
	Fingerprint string // Full SHA256:<base64> fingerprint
	Comment     string
	Key         ed25519.PublicKey
	SourcePath  string
}

// KeyIDOf returns the stable 16-hex-char identifier for a raw Ed25519 public key.
func KeyIDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// FingerprintOf returns a human-verifiable SHA-256 fingerprint string.
func FingerprintOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// GenerateKeyPair creates a fresh Ed25519 keypair encoded in kryx text format.
func GenerateKeyPair(comment string) (pubData, privData []byte, keyID string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	keyID = KeyIDOf(pub)
	comment = strings.TrimSpace(comment)
	if comment == "" {
		comment = "kryx-release-key"
	}

	pubLine := fmt.Sprintf("# %s %s\n%s %s %s\n",
		PubKeyHeader, keyID, PubKeyHeader, base64.StdEncoding.EncodeToString(pub), comment)
	privLine := fmt.Sprintf("# %s %s (KEEP SECRET)\n%s %s %s\n",
		PrivKeyHeader, keyID, PrivKeyHeader, base64.StdEncoding.EncodeToString(priv), comment)

	return []byte(pubLine), []byte(privLine), keyID, nil
}

// ParsePublicKey parses either a kryx-ed25519-pub-v1 key or a standard OpenSSH
// `ssh-ed25519 <base64> [comment]` public key.
func ParsePublicKey(data []byte) (PublicKey, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case PubKeyHeader:
			raw, err := base64.StdEncoding.DecodeString(fields[1])
			if err != nil || len(raw) != ed25519.PublicKeySize {
				return PublicKey{}, fmt.Errorf("invalid %s public key payload", PubKeyHeader)
			}
			pub := ed25519.PublicKey(raw)
			comment := ""
			if len(fields) > 2 {
				comment = strings.Join(fields[2:], " ")
			}
			return PublicKey{
				KeyID:       KeyIDOf(pub),
				Fingerprint: FingerprintOf(pub),
				Comment:     comment,
				Key:         pub,
			}, nil

		case "ssh-ed25519":
			blob, err := base64.StdEncoding.DecodeString(fields[1])
			if err != nil {
				return PublicKey{}, fmt.Errorf("decode ssh-ed25519 key: %w", err)
			}
			pub, err := extractSSHEd25519PubKey(blob)
			if err != nil {
				return PublicKey{}, err
			}
			comment := ""
			if len(fields) > 2 {
				comment = strings.Join(fields[2:], " ")
			}
			return PublicKey{
				KeyID:       KeyIDOf(pub),
				Fingerprint: FingerprintOf(pub),
				Comment:     comment,
				Key:         pub,
			}, nil
		}
	}
	return PublicKey{}, fmt.Errorf("no valid kryx-ed25519 or ssh-ed25519 public key found")
}

// extractSSHEd25519PubKey decodes the RFC 4253 / OpenSSH wire format:
// uint32(len("ssh-ed25519")) || "ssh-ed25519" || uint32(32) || <32-byte key>.
func extractSSHEd25519PubKey(blob []byte) (ed25519.PublicKey, error) {
	if len(blob) < 4 {
		return nil, errors.New("truncated ssh-ed25519 blob")
	}
	tlen := int(binary.BigEndian.Uint32(blob[:4]))
	blob = blob[4:]
	if len(blob) < tlen+4 || string(blob[:tlen]) != "ssh-ed25519" {
		return nil, errors.New("invalid ssh-ed25519 key type header")
	}
	blob = blob[tlen:]
	klen := int(binary.BigEndian.Uint32(blob[:4]))
	blob = blob[4:]
	if klen != ed25519.PublicKeySize || len(blob) < klen {
		return nil, fmt.Errorf("invalid ssh-ed25519 key length %d", klen)
	}
	return ed25519.PublicKey(append([]byte(nil), blob[:klen]...)), nil
}

// ParsePrivateKey parses a kryx-ed25519-key-v1 private key.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == PrivKeyHeader {
			raw, err := base64.StdEncoding.DecodeString(fields[1])
			if err != nil || len(raw) != ed25519.PrivateKeySize {
				return nil, fmt.Errorf("invalid %s private key payload", PrivKeyHeader)
			}
			return ed25519.PrivateKey(raw), nil
		}
	}
	return nil, fmt.Errorf("no valid %s private key found", PrivKeyHeader)
}

// SignBytes signs payload with priv and returns an armored detached signature.
func SignBytes(priv ed25519.PrivateKey, payload []byte) ([]byte, string) {
	pub := priv.Public().(ed25519.PublicKey)
	keyID := KeyIDOf(pub)
	sig := ed25519.Sign(priv, payload)
	out := fmt.Sprintf("# %s\nkey-id: %s\nsig: %s\n",
		SigHeader, keyID, base64.StdEncoding.EncodeToString(sig))
	return []byte(out), keyID
}

// SignFile reads targetPath, signs it using privKeyPath, and writes sigPath atomically.
func SignFile(privKeyPath, targetPath, sigPath string) (string, error) {
	privData, err := os.ReadFile(privKeyPath)
	if err != nil {
		return "", fmt.Errorf("read private key %s: %w", privKeyPath, err)
	}
	priv, err := ParsePrivateKey(privData)
	if err != nil {
		return "", err
	}
	payload, err := os.ReadFile(targetPath)
	if err != nil {
		return "", fmt.Errorf("read target %s: %w", targetPath, err)
	}
	sigData, keyID := SignBytes(priv, payload)
	if err := os.WriteFile(sigPath, sigData, 0o644); err != nil {
		return "", fmt.Errorf("write signature %s: %w", sigPath, err)
	}
	return keyID, nil
}

// LoadKeyring loads all trusted *.pub public keys from keysDir.
func LoadKeyring(keysDir string) ([]PublicKey, error) {
	entries, err := os.ReadDir(keysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read keyring dir %s: %w", keysDir, err)
	}
	var keys []PublicKey
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(keysDir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		pk, err := ParsePublicKey(data)
		if err != nil {
			continue
		}
		pk.Name = strings.TrimSuffix(e.Name(), ".pub")
		pk.SourcePath = full
		keys = append(keys, pk)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	return keys, nil
}

// VerifyBytes checks sigData over payload against any trusted key in keys.
// Supports seamless key rotation when multiple public keys are trusted.
func VerifyBytes(keys []PublicKey, payload, sigData []byte) (PublicKey, error) {
	if len(keys) == 0 {
		return PublicKey{}, errors.New("no trusted public keys in keyring (add one with: pe key trust <key.pub>)")
	}
	sigKeyID, rawSig, err := parseSignature(sigData)
	if err != nil {
		return PublicKey{}, err
	}

	// Try exact key-id match first, then fall back to all trusted keys.
	for _, k := range keys {
		if sigKeyID != "" && k.KeyID == sigKeyID {
			if ed25519.Verify(k.Key, payload, rawSig) {
				return k, nil
			}
		}
	}
	for _, k := range keys {
		if ed25519.Verify(k.Key, payload, rawSig) {
			return k, nil
		}
	}
	if sigKeyID != "" {
		return PublicKey{}, fmt.Errorf("signature (key-id %s) not trusted by any key in keyring; if the repository rotated keys, trust the new public key with: pe key trust <new.pub>", sigKeyID)
	}
	return PublicKey{}, errors.New("signature verification failed against all trusted keys")
}

// VerifyFile verifies sigPath over targetPath using all *.pub keys in keysDir.
func VerifyFile(keysDir, targetPath, sigPath string) (PublicKey, error) {
	keys, err := LoadKeyring(keysDir)
	if err != nil {
		return PublicKey{}, err
	}
	payload, err := os.ReadFile(targetPath)
	if err != nil {
		return PublicKey{}, fmt.Errorf("read %s: %w", targetPath, err)
	}
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		return PublicKey{}, fmt.Errorf("read signature %s: %w", sigPath, err)
	}
	return VerifyBytes(keys, payload, sigData)
}

func parseSignature(sigData []byte) (keyID string, rawSig []byte, err error) {
	scanner := bufio.NewScanner(strings.NewReader(string(sigData)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "key-id:") {
			keyID = strings.TrimSpace(strings.TrimPrefix(line, "key-id:"))
		} else if strings.HasPrefix(line, "sig:") {
			b64 := strings.TrimSpace(strings.TrimPrefix(line, "sig:"))
			decoded, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil || len(decoded) != ed25519.SignatureSize {
				return "", nil, errors.New("malformed ed25519 signature bytes")
			}
			rawSig = decoded
		}
	}
	if len(rawSig) == 0 {
		return "", nil, errors.New("signature file missing 'sig:' line")
	}
	return keyID, rawSig, nil
}
