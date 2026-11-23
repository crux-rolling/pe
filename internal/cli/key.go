package cli

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kryx-linux/pe/internal/config"
	"github.com/kryx-linux/pe/internal/repo"
	"github.com/kryx-linux/pe/internal/signing"
)

// cmdKey manages trusted Ed25519 release signing keys under [paths] keys_dir
// (/etc/pe/keys.d/*.pub), supporting key generation, listing, trusting
// (local file or HTTPS URL for key rotation/repo migration), and revocation.
func cmdKey(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	if len(args) == 0 {
		printKeyUsage(stdout)
		return nil
	}
	sub := args[0]
	rest := args[1:]
	keysDir := cfg.Paths.KeysDir
	if keysDir == "" {
		keysDir = "/etc/pe/keys.d"
	}

	switch sub {
	case "help", "-h", "--help":
		printKeyUsage(stdout)
		return nil

	case "gen", "generate":
		fs := flag.NewFlagSet("key gen", flag.ContinueOnError)
		fs.SetOutput(stderr)
		outPrefix := fs.String("out", "kryx-release", "output filename prefix (<prefix>.pub and <prefix>.key)")
		comment := fs.String("comment", "kryx-release-key", "key comment / owner label")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pubData, privData, keyID, err := signing.GenerateKeyPair(*comment)
		if err != nil {
			return err
		}
		pubPath := *outPrefix + ".pub"
		privPath := *outPrefix + ".key"
		if err := os.MkdirAll(filepath.Dir(pubPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(pubPath, pubData, 0o644); err != nil {
			return fmt.Errorf("write public key %s: %w", pubPath, err)
		}
		if err := os.WriteFile(privPath, privData, 0o600); err != nil {
			return fmt.Errorf("write private key %s: %w", privPath, err)
		}
		pk, _ := signing.ParsePublicKey(pubData)
		fmt.Fprintf(stdout, "%s generated Ed25519 keypair\n", styles.out.BoldGreen("==>"))
		fmt.Fprintf(stdout, "  Key ID:      %s\n", keyID)
		fmt.Fprintf(stdout, "  Fingerprint: %s\n", pk.Fingerprint)
		fmt.Fprintf(stdout, "  Public key:  %s\n", pubPath)
		fmt.Fprintf(stdout, "  Private key: %s (mode 0600 — keep offline/secret!)\n", privPath)
		return nil

	case "list", "ls":
		keys, err := signing.LoadKeyring(keysDir)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			fmt.Fprintf(stdout, "No trusted keys in %s (add one with: pe key trust <file-or-url>)\n", keysDir)
			return nil
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			styles.out.Bold("Name"),
			styles.out.Bold("KeyID"),
			styles.out.Bold("Fingerprint"),
			styles.out.Bold("Comment"),
		)
		for _, k := range keys {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Name, k.KeyID, k.Fingerprint, k.Comment)
		}
		return tw.Flush()

	case "trust", "add":
		fs := flag.NewFlagSet("key trust", flag.ContinueOnError)
		fs.SetOutput(stderr)
		nameFlag := fs.String("name", "", "local key filename in keys.d (without .pub)")
		if err := parseInterspersedFlags(fs, rest); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: pe key trust [--name <name>] <pubkey-file-or-https-url>")
		}
		src := fs.Arg(0)
		var data []byte
		var err error
		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
			if err := repo.ValidateHTTPURL(src); err != nil {
				return err
			}
			client := &http.Client{Timeout: time.Duration(cfg.Repo.TimeoutSeconds) * time.Second}
			resp, rerr := client.Get(src)
			if rerr != nil {
				return fmt.Errorf("download public key %s: %w", src, rerr)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("download public key %s: HTTP %s", src, resp.Status)
			}
			data, err = io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if err != nil {
				return err
			}
		} else {
			data, err = os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("read public key %s: %w", src, err)
			}
		}

		pk, err := signing.ParsePublicKey(data)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(*nameFlag)
		if name == "" {
			base := filepath.Base(src)
			base = strings.TrimSuffix(base, ".pub")
			if base == "" || base == "." || base == "/" {
				base = pk.KeyID
			}
			name = base
		}
		if err := os.MkdirAll(keysDir, 0o755); err != nil {
			return fmt.Errorf("create keys dir %s: %w", keysDir, err)
		}
		dst := filepath.Join(keysDir, name+".pub")
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("install public key %s: %w", dst, err)
		}
		fmt.Fprintf(stdout, "%s trusted key %s (%s) -> %s\n",
			styles.out.BoldGreen("trusted"), styles.out.Bold(pk.KeyID), pk.Fingerprint, dst)
		return nil

	case "revoke", "rm", "remove":
		if len(rest) != 1 {
			return fmt.Errorf("usage: pe key revoke <name-or-keyid>")
		}
		target := strings.TrimSuffix(strings.TrimSpace(rest[0]), ".pub")
		keys, err := signing.LoadKeyring(keysDir)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if k.Name == target || k.KeyID == target {
				if err := os.Remove(k.SourcePath); err != nil {
					return fmt.Errorf("remove key %s: %w", k.SourcePath, err)
				}
				fmt.Fprintf(stdout, "%s revoked key %s (%s)\n",
					styles.out.BoldYellow("revoked"), k.Name, k.KeyID)
				return nil
			}
		}
		return fmt.Errorf("key %q not found in %s", target, keysDir)

	default:
		return fmt.Errorf("unknown key subcommand %q (expected: gen, list, trust, revoke)", sub)
	}
}

func printKeyUsage(w io.Writer) {
	fmt.Fprint(w, `usage: pe key <subcommand> [options]

Manage trusted Ed25519 release signing keys in /etc/pe/keys.d/*.pub.
Accepts both kryx-ed25519 and OpenSSH ssh-ed25519 public keys.

Subcommands:
  gen [--out prefix] [--comment text]   Generate a new Ed25519 signing keypair
  list                                  List trusted public keys and fingerprints
  trust [--name name] <file-or-url>     Add a trusted public key (for setup or key rotation)
  revoke <name-or-keyid>                Remove a retired or compromised key
`)
}

// fetchAndVerifyIndexSig downloads <indexURL>.sig and verifies it against keysDir.
func fetchAndVerifyIndexSig(indexURL, localIndexPath, keysDir string, timeout time.Duration) (signing.PublicKey, error) {
	sigURL := indexURL + ".sig"
	if err := repo.ValidateHTTPURL(sigURL); err != nil {
		return signing.PublicKey{}, err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(sigURL)
	if err != nil {
		return signing.PublicKey{}, fmt.Errorf("download signature %s: %w", sigURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return signing.PublicKey{}, fmt.Errorf("signature %s unavailable (HTTP %s); pass --allow-untrusted to bypass during initial key setup", sigURL, resp.Status)
	}
	sigBytes, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return signing.PublicKey{}, fmt.Errorf("read signature %s: %w", sigURL, err)
	}
	sigPath := localIndexPath + ".sig"
	_ = os.WriteFile(sigPath, sigBytes, 0o644)

	keys, err := signing.LoadKeyring(keysDir)
	if err != nil {
		return signing.PublicKey{}, err
	}
	payload, err := os.ReadFile(localIndexPath)
	if err != nil {
		return signing.PublicKey{}, err
	}
	return signing.VerifyBytes(keys, payload, sigBytes)
}
