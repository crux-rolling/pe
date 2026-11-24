package sources

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// DefaultKPMConfigPath is the default path to kpm's INI configuration file.
const DefaultKPMConfigPath = "/etc/kpm.ini"

// KPMConfig holds the subset of /etc/kpm.ini settings shared between kpm and ports.
type KPMConfig struct {
	Root        string
	PortsDir    string
	Collections []string
	Color       string
}

// DefaultKPMConfig returns kpm's documented CRUX-compatible defaults.
func DefaultKPMConfig() KPMConfig {
	return KPMConfig{
		Root:        "/",
		PortsDir:    DefaultPortsDir,
		Collections: []string{"core", "opt", "xorg", "contrib"},
		Color:       "auto",
	}
}

// ResolveKPMConfigPath chooses the kpm.ini path from explicit flag, $KPM_CONFIG, or default.
func ResolveKPMConfigPath(explicit string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	if env := strings.TrimSpace(os.Getenv("KPM_CONFIG")); env != "" {
		return env
	}
	return DefaultKPMConfigPath
}

// LoadKPMConfig reads /etc/kpm.ini (or an explicit override) layered over DefaultKPMConfig().
// If explicit is false and the file does not exist, defaults are returned with no error.
func LoadKPMConfig(path string, explicit bool) (KPMConfig, error) {
	cfg := DefaultKPMConfig()
	if strings.TrimSpace(path) == "" {
		path = DefaultKPMConfigPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read kpm config %q: %w", path, err)
	}
	if err := ParseKPMConfig(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("parse kpm config %q: %w", path, err)
	}
	return cfg, nil
}

// ParseKPMConfig parses kpm.ini content into cfg, ignoring sections/keys unrelated to ports
// while honoring kpm's exact comment, quoting, and list conventions.
func ParseKPMConfig(input string, cfg *KPMConfig) error {
	scanner := bufio.NewScanner(strings.NewReader(input))
	section := ""
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				return fmt.Errorf("line %d: unclosed section header %q", lineNum, line)
			}
			section = strings.ToLower(strings.TrimSpace(line[1:end]))
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return fmt.Errorf("line %d: expected key = value, got %q", lineNum, line)
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		val := unquoteINI(stripINIInlineComment(strings.TrimSpace(line[eq+1:])))

		switch section {
		case "paths", "":
			switch key {
			case "root":
				if val != "" {
					cfg.Root = val
				}
			case "ports_dir":
				if val != "" {
					cfg.PortsDir = val
				}
			case "collections":
				cfg.Collections = splitINIList(val)
			}
		case "ui":
			if key == "color" && val != "" {
				cfg.Color = strings.ToLower(val)
			}
		}
	}
	return scanner.Err()
}

func splitINIList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func stripINIInlineComment(s string) string {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#', ';':
			if !inSingle && !inDouble && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return strings.TrimSpace(s)
}

func unquoteINI(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
