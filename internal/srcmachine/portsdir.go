package srcmachine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kryx-linux/kpm/internal/config"
)

// ResolvePortsDir resolves the CRUX/KryX ports directory using a strict 5-step
// priority order so kryx-src never hardcodes or blindly assumes /usr/ports
// (especially inside Docker/CI builds):
//
//  1. Explicit CLI flag (--ports-dir / -P / --ports)
//  2. Environment variable: KRYX_PORTS_DIR, then KPM_PORTS_DIR
//  3. Active config file ([paths] ports_dir in kpm.ini, when explicitly configured or present on disk)
//  4. Workspace auto-detection in cwd:
//     - ./Pkgfile or ./.kryx/Pkgfile (single-port repo)
//     - ./ports directory
//     - collection directories (./core, ./opt, ./xorg, ./contrib, ./rolling) in cwd
//  5. Fallback to /usr/ports ONLY if /usr/ports exists on disk; otherwise return
//     an actionable error explaining how to pass --ports-dir or set KRYX_PORTS_DIR.
func ResolvePortsDir(cliPortsDir, kpmConfigPath, cwd string) (string, error) {
	// 1. CLI flag
	if p := strings.TrimSpace(cliPortsDir); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("resolve --ports-dir %q: %w", p, err)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("ports directory %q specified via --ports-dir does not exist or is not a directory", abs)
		}
		return abs, nil
	}

	// 2. Environment variables: KRYX_PORTS_DIR, then KPM_PORTS_DIR
	for _, envKey := range []string{"KRYX_PORTS_DIR", "KPM_PORTS_DIR"} {
		if p := strings.TrimSpace(os.Getenv(envKey)); p != "" {
			abs, err := filepath.Abs(p)
			if err != nil {
				return "", fmt.Errorf("resolve %s=%q: %w", envKey, p, err)
			}
			if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
				return "", fmt.Errorf("ports directory %q specified via %s does not exist or is not a directory", abs, envKey)
			}
			return abs, nil
		}
	}

	// 3. Active config file ([paths] ports_dir in kpm.ini)
	cfgPath := config.ResolvePath(kpmConfigPath)
	explicitCfg := strings.TrimSpace(kpmConfigPath) != "" || strings.TrimSpace(os.Getenv("KPM_CONFIG")) != ""
	if fi, err := os.Stat(cfgPath); err == nil && !fi.IsDir() {
		cfg, err := config.Load(cfgPath, explicitCfg)
		if err != nil && explicitCfg {
			return "", err
		}
		if err == nil {
			for _, dir := range cfg.EffectivePortsDirs() {
				dir = strings.TrimSpace(dir)
				if dir == "" {
					continue
				}
				// If a non-explicit default /etc/kpm.ini merely contains the default
				// /usr/ports and that directory does not exist, defer to workspace
				// auto-detection before failing.
				if dfi, serr := os.Stat(dir); serr == nil && dfi.IsDir() {
					if abs, aerr := filepath.Abs(dir); aerr == nil {
						return abs, nil
					}
					return dir, nil
				}
			}
		}
	} else if explicitCfg {
		return "", fmt.Errorf("config file %q not found", cfgPath)
	}

	// 4. Workspace auto-detection in current working directory (e.g. /workspace in Docker)
	if strings.TrimSpace(cwd) == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	if cwd != "" {
		if absCwd, err := filepath.Abs(cwd); err == nil {
			cwd = absCwd
		}
		// 4a. Single-port repo at cwd (./Pkgfile or ./.kryx/Pkgfile)
		if isRegularFile(filepath.Join(cwd, "Pkgfile")) || isRegularFile(filepath.Join(cwd, ".kryx", "Pkgfile")) {
			return cwd, nil
		}
		// 4b. ./ports directory inside workspace
		subPorts := filepath.Join(cwd, "ports")
		if isDir(subPorts) {
			return subPorts, nil
		}
		// 4c. Standard collection directories directly under workspace
		for _, col := range []string{"core", "opt", "xorg", "contrib", "pkg-tools", "kernel", "rolling"} {
			if isDir(filepath.Join(cwd, col)) {
				return cwd, nil
			}
		}
	}

	// 5. Fallback to /usr/ports ONLY if it exists on disk
	if isDir("/usr/ports") {
		return "/usr/ports", nil
	}

	return "", fmt.Errorf(
		"unable to locate ports directory (checked --ports-dir, $KRYX_PORTS_DIR, $KPM_PORTS_DIR, kpm.ini, workspace %q, and /usr/ports); "+
			"specify --ports-dir <path> (or -P <path>) or export KRYX_PORTS_DIR=<path>",
		cwd,
	)
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
