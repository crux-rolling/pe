// Package signing implements pure-Go Ed25519 release signing and keyring
// verification supporting:
//   - OpenBSD signify keys & detached signatures (.pub / .sec / .sig)
//   - Native kryx-ed25519 keys & signatures
//   - OpenSSH ssh-ed25519 public keys
//
// Supports multi-key rotation, recovery, and single-pubkey pinning.
package signing

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
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

const (
	signifyPubLen = 2 + 8 + ed25519.PublicKeySize  // "Ed" + keynum(8) + pub(32) = 42
	signifySigLen = 2 + 8 + ed25519.SignatureSize  // "Ed" + keynum(8) + sig(64) = 74
	signifySecLen = 2 + 2 + 4 + 16 + 8 + 8 + ed25519.PrivateKeySize // 104
)

// PublicKey represents a trusted Ed25519 verification key loaded from keys.d or [repo] pubkey.
type PublicKey struct {
	Name          string
	KeyID         string // First 16 hex chars of SHA-256(raw 32-byte pubkey)
	SignifyKeyNum [8]byte
	HasKeyNum     bool
	Fingerprint   string // Full SHA256:<base64> fingerprint
	Comment       string
	Key           ed25519.PublicKey
	SourcePath    string
}

// KeyIDOf returns the stable 16-hex-char identifier for a raw Ed25519 public key.
func KeyIDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// DefaultKeyNum derives a deterministic 8-byte signify keynum from a public key.
func DefaultKeyNum(pub ed25519.PublicKey) [8]byte {
	sum := sha256.Sum256(pub)
	var kn [8]byte
	copy(kn[:], sum[:8])
	return kn
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

// GenerateSignifyKeyPair creates a fresh Ed25519 keypair encoded in standard
// OpenBSD signify format (unencrypted .sec and .pub).
func GenerateSignifyKeyPair(comment string) (pubData, secData []byte, keyID string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	var keynum [8]byte
	if _, err := rand.Read(keynum[:]); err != nil {
		return nil, nil, "", fmt.Errorf("generate signify keynum: %w", err)
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, nil, "", fmt.Errorf("generate signify salt: %w", err)
	}

	comment = strings.TrimSpace(comment)
	if comment == "" {
		comment = "kryx-release"
	}
	keyID = KeyIDOf(pub)

	pubPayload := make([]byte, 0, signifyPubLen)
	pubPayload = append(pubPayload, 'E', 'd')
	pubPayload = append(pubPayload, keynum[:]...)
	pubPayload = append(pubPayload, pub...)
	pubOut := fmt.Sprintf("untrusted comment: %s public key\n%s\n",
		comment, base64.StdEncoding.EncodeToString(pubPayload))

	chk := sha512.Sum512(priv)
	secPayload := make([]byte, 0, signifySecLen)
	secPayload = append(secPayload, 'E', 'd')
	secPayload = append(secPayload, 'B', 'K')
	secPayload = append(secPayload, 0, 0, 0, 0) // kdfrounds = 0 (unencrypted)
	secPayload = append(secPayload, salt[:]...)
	secPayload = append(secPayload, chk[:8]...)
	secPayload = append(secPayload, keynum[:]...)
	secPayload = append(secPayload, priv...)
	secOut := fmt.Sprintf("untrusted comment: %s secret key\n%s\n",
		comment, base64.StdEncoding.EncodeToString(secPayload))

	return []byte(pubOut), []byte(secOut), keyID, nil
}

// ParsePublicKey parses a kryx-ed25519-pub-v1 key, an OpenSSH `ssh-ed25519` key,
// or an OpenBSD `signify` public key (`untrusted comment:` + 42-byte `"Ed"` blob).
func ParsePublicKey(data []byte) (PublicKey, error) {
	var untrustedComment string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "untrusted comment:") {
			untrustedComment = strings.TrimSpace(strings.TrimPrefix(line, "untrusted comment:"))
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 1 {
			// Try OpenBSD signify base64 public key line.
			raw, err := base64.StdEncoding.DecodeString(fields[0])
			if err == nil && len(raw) == signifyPubLen && raw[0] == 'E' && raw[1] == 'd' {
				var kn [8]byte
				copy(kn[:], raw[2:10])
				pub := ed25519.PublicKey(append([]byte(nil), raw[10:42]...))
				return PublicKey{
					KeyID:         KeyIDOf(pub),
					SignifyKeyNum: kn,
					HasKeyNum:     true,
					Fingerprint:   FingerprintOf(pub),
					Comment:       untrustedComment,
					Key:           pub,
				}, nil
			}
			continue
		}
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
	return PublicKey{}, fmt.Errorf("no valid signify, kryx-ed25519, or ssh-ed25519 public key found")
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

// ParsePrivateKey parses either a kryx-ed25519-key-v1 private key or an
// unencrypted OpenBSD signify secret key (.sec).
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	priv, _, _, err := ParsePrivateKeyWithKeyNum(data)
	return priv, err
}

// ParsePrivateKeyWithKeyNum parses a private key and also returns its signify
// 8-byte keynum (or a deterministic keynum derived from the public key).
func ParsePrivateKeyWithKeyNum(data []byte) (ed25519.PrivateKey, [8]byte, bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 1 {
			// OpenBSD signify secret key format
			raw, err := base64.StdEncoding.DecodeString(fields[0])
			if err == nil && len(raw) == signifySecLen && raw[0] == 'E' && raw[1] == 'd' && raw[2] == 'B' && raw[3] == 'K' {
				rounds := binary.BigEndian.Uint32(raw[4:8])
				if rounds != 0 {
					return nil, [8]byte{}, false, errors.New("passphrase-encrypted signify secret keys are not supported in non-interactive mode (generate with signify -G -n or kpm key gen)")
				}
				expectedChk := raw[24:32]
				var kn [8]byte
				copy(kn[:], raw[32:40])
				sec := ed25519.PrivateKey(append([]byte(nil), raw[40:104]...))
				actualChk := sha512.Sum512(sec)
				if !bytes.Equal(expectedChk, actualChk[:8]) {
					return nil, [8]byte{}, false, errors.New("signify secret key checksum verification failed")
				}
				return sec, kn, true, nil
			}
			continue
		}
		if len(fields) >= 2 && fields[0] == PrivKeyHeader {
			raw, err := base64.StdEncoding.DecodeString(fields[1])
			if err != nil || len(raw) != ed25519.PrivateKeySize {
				return nil, [8]byte{}, false, fmt.Errorf("invalid %s private key payload", PrivKeyHeader)
			}
			priv := ed25519.PrivateKey(raw)
			pub := priv.Public().(ed25519.PublicKey)
			return priv, DefaultKeyNum(pub), false, nil
		}
	}
	return nil, [8]byte{}, false, fmt.Errorf("no valid signify or %s private key found", PrivKeyHeader)
}

// SignBytes signs payload with priv and returns an armored kryx-ed25519 detached signature.
func SignBytes(priv ed25519.PrivateKey, payload []byte) ([]byte, string) {
	pub := priv.Public().(ed25519.PublicKey)
	keyID := KeyIDOf(pub)
	sig := ed25519.Sign(priv, payload)
	out := fmt.Sprintf("# %s\nkey-id: %s\nsig: %s\n",
		SigHeader, keyID, base64.StdEncoding.EncodeToString(sig))
	return []byte(out), keyID
}

// SignBytesSignify signs payload with priv and returns an OpenBSD signify-compatible
// detached signature (`untrusted comment:` + base64 `"Ed" || keynum(8) || sig(64)`).
func SignBytesSignify(priv ed25519.PrivateKey, keynum [8]byte, comment string, payload []byte) ([]byte, string) {
	pub := priv.Public().(ed25519.PublicKey)
	keyID := KeyIDOf(pub)
	sig := ed25519.Sign(priv, payload)
	blob := make([]byte, 0, signifySigLen)
	blob = append(blob, 'E', 'd')
	blob = append(blob, keynum[:]...)
	blob = append(blob, sig...)
	comment = strings.TrimSpace(comment)
	if comment == "" {
		comment = fmt.Sprintf("signature from kryx key %s", keyID)
	}
	out := fmt.Sprintf("untrusted comment: %s\n%s\n", comment, base64.StdEncoding.EncodeToString(blob))
	return []byte(out), keyID
}

// SignFile reads targetPath, signs it using privKeyPath, and writes sigPath.
// If privKeyPath is an OpenBSD signify secret key, writes a signify signature;
// otherwise writes a kryx-ed25519 signature.
func SignFile(privKeyPath, targetPath, sigPath string) (string, error) {
	privData, err := os.ReadFile(privKeyPath)
	if err != nil {
		return "", fmt.Errorf("read private key %s: %w", privKeyPath, err)
	}
	priv, keynum, isSignify, err := ParsePrivateKeyWithKeyNum(privData)
	if err != nil {
		return "", err
	}
	payload, err := os.ReadFile(targetPath)
	if err != nil {
		return "", fmt.Errorf("read target %s: %w", targetPath, err)
	}
	var sigData []byte
	var keyID string
	if isSignify {
		sigData, keyID = SignBytesSignify(priv, keynum, "verify with "+filepath.Base(privKeyPath), payload)
	} else {
		sigData, keyID = SignBytes(priv, payload)
	}
	if err := writeAtomic(sigPath, sigData, 0o644); err != nil {
		return "", fmt.Errorf("write signature %s: %w", sigPath, err)
	}
	return keyID, nil
}

// SignFileSignify reads targetPath, signs it using privKeyPath (accepting either
// signify .sec or kryx-ed25519 .key private keys), and always writes a standard
// OpenBSD signify-compatible detached signature to sigPath.
func SignFileSignify(privKeyPath, targetPath, sigPath string) (string, error) {
	privData, err := os.ReadFile(privKeyPath)
	if err != nil {
		return "", fmt.Errorf("read private key %s: %w", privKeyPath, err)
	}
	priv, keynum, _, err := ParsePrivateKeyWithKeyNum(privData)
	if err != nil {
		return "", err
	}
	payload, err := os.ReadFile(targetPath)
	if err != nil {
		return "", fmt.Errorf("read target %s: %w", targetPath, err)
	}
	sigData, keyID := SignBytesSignify(priv, keynum, "verify with kryx repository public key", payload)
	if err := writeAtomic(sigPath, sigData, 0o644); err != nil {
		return "", fmt.Errorf("write signature %s: %w", sigPath, err)
	}
	return keyID, nil
}

// LoadPublicKeyFile loads a single public key file (signify, kryx-ed25519, or ssh-ed25519).
func LoadPublicKeyFile(path string) (PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PublicKey{}, fmt.Errorf("read public key %s: %w", path, err)
	}
	pk, err := ParsePublicKey(data)
	if err != nil {
		return PublicKey{}, fmt.Errorf("parse public key %s: %w", path, err)
	}
	pk.Name = strings.TrimSuffix(filepath.Base(path), ".pub")
	pk.SourcePath = path
	return pk, nil
}

// LoadKeyring loads all trusted *.pub public keys from keysDir.
func LoadKeyring(keysDir string) ([]PublicKey, error) {
	if strings.TrimSpace(keysDir) == "" {
		return nil, nil
	}
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

// LoadVerificationKeys loads an explicit [repo] pubkey (if set) plus all trusted
// keys from keysDir. When pubKeyPath is specified and unreadable/invalid, an error is returned.
func LoadVerificationKeys(pubKeyPath, keysDir string) ([]PublicKey, error) {
	var keys []PublicKey
	if strings.TrimSpace(pubKeyPath) != "" {
		pk, err := LoadPublicKeyFile(pubKeyPath)
		if err != nil {
			return nil, err
		}
		keys = append(keys, pk)
	}
	if strings.TrimSpace(keysDir) != "" {
		ring, err := LoadKeyring(keysDir)
		if err != nil {
			return nil, err
		}
		keys = append(keys, ring...)
	}
	return keys, nil
}

// VerifyBytes checks sigData (either OpenBSD signify or kryx-ed25519 format) over
// payload against any trusted key in keys.
func VerifyBytes(keys []PublicKey, payload, sigData []byte) (PublicKey, error) {
	if len(keys) == 0 {
		return PublicKey{}, errors.New("no trusted public keys configured (set [repo] pubkey or run: kpm key trust <key.pub>)")
	}
	sigInfo, err := parseSignature(sigData)
	if err != nil {
		return PublicKey{}, err
	}

	// 1. Prefer exact signify keynum match when present.
	if sigInfo.hasKeyNum {
		for _, k := range keys {
			if k.HasKeyNum && k.SignifyKeyNum == sigInfo.keyNum {
				if ed25519.Verify(k.Key, payload, sigInfo.rawSig) {
					return k, nil
				}
			}
		}
	}
	// 2. Prefer exact kryx key-id match when present.
	if sigInfo.keyID != "" {
		for _, k := range keys {
			if k.KeyID == sigInfo.keyID {
				if ed25519.Verify(k.Key, payload, sigInfo.rawSig) {
					return k, nil
				}
			}
		}
	}
	// 3. Fall back to trying every trusted key (supports keynum-less / rotated keys).
	for _, k := range keys {
		if ed25519.Verify(k.Key, payload, sigInfo.rawSig) {
			return k, nil
		}
	}

	if sigInfo.keyID != "" {
		return PublicKey{}, fmt.Errorf("signature (key-id %s) not trusted by any key in keyring; if the repository rotated keys, trust the new public key with: kpm key trust <new.pub>", sigInfo.keyID)
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

type parsedSig struct {
	keyID     string
	keyNum    [8]byte
	hasKeyNum bool
	rawSig    []byte
}

func parseSignature(sigData []byte) (parsedSig, error) {
	var res parsedSig
	scanner := bufio.NewScanner(strings.NewReader(string(sigData)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		if strings.HasPrefix(line, "key-id:") {
			res.keyID = strings.TrimSpace(strings.TrimPrefix(line, "key-id:"))
		} else if strings.HasPrefix(line, "sig:") {
			b64 := strings.TrimSpace(strings.TrimPrefix(line, "sig:"))
			decoded, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil || len(decoded) != ed25519.SignatureSize {
				return parsedSig{}, errors.New("malformed ed25519 signature bytes")
			}
			res.rawSig = decoded
		} else if !strings.ContainsAny(line, " \t:") {
			// OpenBSD signify detached signature payload: base64("Ed" + keynum(8) + sig(64))
			decoded, derr := base64.StdEncoding.DecodeString(line)
			if derr == nil && len(decoded) == signifySigLen && decoded[0] == 'E' && decoded[1] == 'd' {
				copy(res.keyNum[:], decoded[2:10])
				res.hasKeyNum = true
				res.rawSig = append([]byte(nil), decoded[10:74]...)
				res.keyID = hex.EncodeToString(res.keyNum[:])
				return res, nil
			}
		}
	}
	if len(res.rawSig) == 0 {
		return parsedSig{}, errors.New("signature file missing valid signify or ed25519 signature payload")
	}
	return res, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".kpm-sig-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
