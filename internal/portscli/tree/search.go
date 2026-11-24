package tree

import (
	"sort"
	"strings"
)

// Search performs a fast, case-insensitive substring search over port names,
// relative paths, and # Description: headers, returning sorted matches.
func Search(all []Port, query string) []Port {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var matches []Port
	for _, p := range all {
		if strings.Contains(strings.ToLower(p.Name), q) ||
			strings.Contains(strings.ToLower(p.RelDir), q) ||
			strings.Contains(strings.ToLower(p.Description), q) {
			matches = append(matches, p)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Name != matches[j].Name {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].RelDir < matches[j].RelDir
	})
	return matches
}
