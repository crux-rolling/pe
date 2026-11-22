package srcmachine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Severity classifies a lint finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Issue describes a single lint diagnostic discovered in a ports tree.
type Issue struct {
	Severity Severity
	Port     string
	Message  string
}

func (i Issue) String() string {
	return fmt.Sprintf("[%s] %s: %s", strings.ToUpper(string(i.Severity)), i.Port, i.Message)
}

// LintReport aggregates all diagnostics from linting a Registry.
type LintReport struct {
	Checked int
	Issues  []Issue
}

// ErrorCount returns the number of error-level issues.
func (lr LintReport) ErrorCount() int {
	n := 0
	for _, it := range lr.Issues {
		if it.Severity == SeverityError {
			n++
		}
	}
	return n
}

// WarningCount returns the number of warning-level issues.
func (lr LintReport) WarningCount() int {
	n := 0
	for _, it := range lr.Issues {
		if it.Severity == SeverityWarning {
			n++
		}
	}
	return n
}

// Lint validates every entry in reg (or a selected subset when targets is non-empty).
// It catches directory-vs-name mismatches (like rolling/linux-6.18 declaring name=kernel),
// missing required fields, duplicate package names within the same collection,
// missing dependency targets, and dependency cycles.
func Lint(reg *Registry, entries []Entry) LintReport {
	if len(entries) == 0 {
		entries = reg.Entries
	}
	report := LintReport{Checked: len(entries)}

	// Track duplicates within the same collection.
	byColName := make(map[string][]string)

	for _, e := range entries {
		p := e.Port
		label := e.RelDir
		if label == "" {
			label = p.Name
		}

		if p.Name == "" {
			report.Issues = append(report.Issues, Issue{
				Severity: SeverityError,
				Port:     label,
				Message:  "Pkgfile missing required variable 'name'",
			})
		}
		if p.Version == "" {
			report.Issues = append(report.Issues, Issue{
				Severity: SeverityError,
				Port:     label,
				Message:  "Pkgfile missing required variable 'version'",
			})
		}
		if p.Release == "" {
			report.Issues = append(report.Issues, Issue{
				Severity: SeverityError,
				Port:     label,
				Message:  "Pkgfile missing required variable 'release'",
			})
		}

		// Directory basename vs Pkgfile name mismatch warning (unless single-port root repo).
		if !reg.IsSinglePort && p.Name != "" && e.Base != "" && p.Name != e.Base {
			report.Issues = append(report.Issues, Issue{
				Severity: SeverityWarning,
				Port:     label,
				Message:  fmt.Sprintf("directory basename %q differs from Pkgfile name=%q (recommend aligning directory and name)", e.Base, p.Name),
			})
		}

		// Check presence of build() declaration in Pkgfile text.
		if data, err := os.ReadFile(filepath.Join(p.Dir, "Pkgfile")); err == nil {
			txt := string(data)
			if !strings.Contains(txt, "build()") && !strings.Contains(txt, "build ()") {
				report.Issues = append(report.Issues, Issue{
					Severity: SeverityError,
					Port:     label,
					Message:  "Pkgfile missing build() function",
				})
			}
		}

		// Verify runtime and make dependencies resolve in the full registry.
		for _, dep := range p.Depends {
			if _, ok := reg.Resolve(dep); !ok {
				report.Issues = append(report.Issues, Issue{
					Severity: SeverityWarning,
					Port:     label,
					Message:  fmt.Sprintf("dependency %q not found in ports tree", dep),
				})
			}
		}
		for _, mdep := range p.MakeDepends {
			if _, ok := reg.Resolve(mdep); !ok {
				report.Issues = append(report.Issues, Issue{
					Severity: SeverityWarning,
					Port:     label,
					Message:  fmt.Sprintf("make dependency %q not found in ports tree (may still be satisfied from binary index)", mdep),
				})
			}
		}

		if p.Name != "" {
			key := p.Collection + "/" + p.Name
			byColName[key] = append(byColName[key], label)
		}
	}

	// Report duplicate package names inside the same collection.
	var dupKeys []string
	for k, dirs := range byColName {
		if len(dirs) > 1 {
			dupKeys = append(dupKeys, k)
		}
	}
	sort.Strings(dupKeys)
	for _, k := range dupKeys {
		dirs := byColName[k]
		sort.Strings(dirs)
		report.Issues = append(report.Issues, Issue{
			Severity: SeverityError,
			Port:     k,
			Message:  fmt.Sprintf("duplicate package name in collection across directories: %s", strings.Join(dirs, ", ")),
		})
	}

	// Detect dependency cycles among inspected entries.
	state := make(map[string]int) // 0=unvisited, 1=visiting, 2=done
	var stack []string
	var checkCycle func(e Entry)
	checkCycle = func(e Entry) {
		state[e.Port.Dir] = 1
		stack = append(stack, e.RelDir)
		for _, dep := range e.Port.AllBuildDepends() {
			de, ok := reg.Resolve(dep)
			if !ok {
				continue
			}
			switch state[de.Port.Dir] {
			case 0:
				checkCycle(de)
			case 1:
				cyclePath := strings.Join(append(append([]string(nil), stack...), de.RelDir), " -> ")
				report.Issues = append(report.Issues, Issue{
					Severity: SeverityWarning,
					Port:     e.RelDir,
					Message:  fmt.Sprintf("dependency cycle detected: %s", cyclePath),
				})
			}
		}
		stack = stack[:len(stack)-1]
		state[e.Port.Dir] = 2
	}
	for _, e := range entries {
		if state[e.Port.Dir] == 0 {
			checkCycle(e)
		}
	}

	return report
}
