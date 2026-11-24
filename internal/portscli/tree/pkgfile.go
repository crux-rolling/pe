package tree

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	reSimpleAssign = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	reVarBrace     = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	reVarPlain     = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
)

// ParsePkgfile parses CRUX Pkgfile header comments and top-level variable
// assignments purely in Go (zero shell-out). It resolves simple $var / ${var}
// references defined prior to build() and falls back to the directory basename
// if `name` is omitted.
func ParsePkgfile(portDir, portsDir string) (Port, error) {
	pkgfile := filepath.Join(portDir, "Pkgfile")
	f, err := os.Open(pkgfile)
	if err != nil {
		return Port{}, fmt.Errorf("open %s: %w", pkgfile, err)
	}
	defer f.Close()

	p := Port{
		Dir:        portDir,
		Collection: inferCollection(portDir, portsDir),
	}

	vars := make(map[string]string)
	inHeaders := true
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)

		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if inHeaders {
				parseHeaderLine(line, &p)
			}
			continue
		}
		inHeaders = false

		if strings.HasPrefix(line, "build()") || strings.HasPrefix(line, "build ()") ||
			strings.HasPrefix(line, "unpack_source()") || strings.HasPrefix(line, "unpack_source ()") {
			break
		}

		// Strip trailing inline comment outside quotes.
		clean := stripShellComment(line)
		m := reSimpleAssign.FindStringSubmatch(clean)
		if len(m) != 3 {
			continue
		}
		key := m[1]
		val := strings.TrimSpace(m[2])
		// Skip bash array declarations like source=(...)
		if strings.HasPrefix(val, "(") {
			continue
		}

		singleQuoted := len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\''
		unq := trimShellQuotes(val)
		if !singleQuoted {
			unq = expandSimpleVars(unq, vars)
		}
		vars[key] = unq

		switch key {
		case "name":
			p.Name = unq
		case "version":
			p.Version = unq
		case "release":
			p.Release = unq
		}
	}
	if err := scanner.Err(); err != nil {
		return Port{}, fmt.Errorf("read %s: %w", pkgfile, err)
	}

	if p.Name == "" {
		p.Name = filepath.Base(portDir)
	}
	return p, nil
}

func parseHeaderLine(line string, p *Port) {
	body := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	colon := strings.IndexByte(body, ':')
	if colon <= 0 {
		return
	}
	key := strings.ToLower(strings.TrimSpace(body[:colon]))
	val := strings.TrimSpace(body[colon+1:])
	switch key {
	case "description":
		p.Description = val
	case "url":
		p.URL = val
	case "maintainer":
		p.Maintainer = val
	case "packager":
		p.Packager = val
	case "depends on", "depends":
		p.Depends = appendUnique(p.Depends, parseDepends(val)...)
	case "make depends on", "make depends", "makedepends", "build depends on", "build depends":
		p.MakeDepends = appendUnique(p.MakeDepends, parseDepends(val)...)
	}
}

func appendUnique(dst []string, items ...string) []string {
	seen := make(map[string]bool, len(dst)+len(items))
	for _, s := range dst {
		seen[s] = true
	}
	for _, item := range items {
		if item != "" && !seen[item] {
			seen[item] = true
			dst = append(dst, item)
		}
	}
	return dst
}

func parseDepends(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	seen := make(map[string]bool, len(fields))
	var deps []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] {
			seen[f] = true
			deps = append(deps, f)
		}
	}
	return deps
}

func expandSimpleVars(s string, vars map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	out := reVarBrace.ReplaceAllStringFunc(s, func(tok string) string {
		name := tok[2 : len(tok)-1]
		if v, ok := vars[name]; ok {
			return v
		}
		return tok
	})
	out = reVarPlain.ReplaceAllStringFunc(out, func(tok string) string {
		name := tok[1:]
		if v, ok := vars[name]; ok {
			return v
		}
		return tok
	})
	return out
}

func stripShellComment(s string) string {
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
		case '#':
			if !inSingle && !inDouble && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

func trimShellQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func inferCollection(portDir, portsDir string) string {
	if portsDir == "" {
		return ""
	}
	rel, err := filepath.Rel(portsDir, portDir)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[:len(parts)-1], "/")
	}
	return ""
}
