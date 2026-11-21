package pkgdb

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RuleEvent matches pkgadd's INSTALL and UPGRADE rule events.
type RuleEvent int

const (
	EventInstall RuleEvent = iota
	EventUpgrade
)

// Rule represents a single line in /etc/pkgadd.conf.
type Rule struct {
	Event   RuleEvent
	Pattern string
	Action  bool // true = YES, false = NO
	re      *regexp.Regexp
}

// ReadRules loads /etc/pkgadd.conf (or an alternate configPath if non-empty).
// If the config file does not exist, an empty rule slice is returned without error
// (matching pkgadd.cc behavior where default rule is YES for everything).
func ReadRules(root, configPath string) ([]Rule, error) {
	filename := configPath
	if strings.TrimSpace(filename) == "" {
		filename = filepath.Join(normalizeRoot(root), PkgaddConf)
	}
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open pkgadd config %s: %w", filename, err)
	}
	defer f.Close()

	var rules []Rule
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: wrong number of arguments, aborting", filename, lineNum)
		}
		var event RuleEvent
		switch fields[0] {
		case "INSTALL":
			event = EventInstall
		case "UPGRADE":
			event = EventUpgrade
		default:
			return nil, fmt.Errorf("%s:%d: '%s' unknown event, aborting", filename, lineNum, fields[0])
		}
		var action bool
		switch fields[2] {
		case "YES":
			action = true
		case "NO":
			action = false
		default:
			return nil, fmt.Errorf("%s:%d: '%s' unknown action, should be YES or NO, aborting", filename, lineNum, fields[2])
		}
		re, err := regexp.Compile(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: error compiling regular expression '%s': %w", filename, lineNum, fields[1], err)
		}
		rules = append(rules, Rule{
			Event:   event,
			Pattern: fields[1],
			Action:  action,
			re:      re,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}

// ApplyInstallRules filters info.Files according to INSTALL rules (last match wins).
// Mutates info.Files to keep only installable files and returns the set of excluded files.
func ApplyInstallRules(info *PkgInfo, rules []Rule) map[string]struct{} {
	installRules := filterRules(rules, EventInstall)
	installSet := make(map[string]struct{}, len(info.Files))
	nonInstallSet := make(map[string]struct{})

	for file := range info.Files {
		installFile := true
		for i := len(installRules) - 1; i >= 0; i-- {
			if installRules[i].re.MatchString(file) {
				installFile = installRules[i].Action
				break
			}
		}
		if installFile {
			installSet[file] = struct{}{}
		} else {
			nonInstallSet[file] = struct{}{}
		}
	}
	info.Files = installSet
	return nonInstallSet
}

// MakeKeepList returns the set of files from files that must be kept during UPGRADE
// (last matching UPGRADE rule has action NO).
func MakeKeepList(files map[string]struct{}, rules []Rule) map[string]struct{} {
	upgradeRules := filterRules(rules, EventUpgrade)
	keep := make(map[string]struct{})

	for file := range files {
		for i := len(upgradeRules) - 1; i >= 0; i-- {
			if upgradeRules[i].re.MatchString(file) {
				if !upgradeRules[i].Action {
					keep[file] = struct{}{}
				}
				break
			}
		}
	}
	return keep
}

// MakeKeepListFromSlice is a convenience wrapper for MakeKeepList operating on a slice.
func MakeKeepListFromSlice(files []string, rules []Rule) map[string]struct{} {
	set := make(map[string]struct{}, len(files))
	for _, f := range files {
		set[f] = struct{}{}
	}
	return MakeKeepList(set, rules)
}

func filterRules(rules []Rule, event RuleEvent) []Rule {
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.Event == event {
			out = append(out, r)
		}
	}
	return out
}
