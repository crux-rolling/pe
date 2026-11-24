package kpmcompat

import (
	"strconv"
	"strings"
)

// SplitVersionRelease splits a CRUX/kpm "<version>-<release>" string on its last '-'.
func SplitVersionRelease(full string) (version, release string) {
	idx := strings.LastIndex(full, "-")
	if idx <= 0 || idx == len(full)-1 {
		return full, ""
	}
	return full[:idx], full[idx+1:]
}

// CompareFullVersion compares two "<version>-<release>" strings using kpm's exact
// numeric-aware segment comparison rules.
// Returns -1 if a < b, 0 if a == b, +1 if a > b.
func CompareFullVersion(a, b string) int {
	if a == b {
		return 0
	}
	va, ra := SplitVersionRelease(a)
	vb, rb := SplitVersionRelease(b)
	return CompareVersion(va, ra, vb, rb)
}

// CompareVersion mirrors kpm/internal/repo.CompareVersion exactly:
// compares version segments first, then release segments.
func CompareVersion(verA, relA, verB, relB string) int {
	if cmp := compareSegments(verA, verB); cmp != 0 {
		return cmp
	}
	return compareSegments(relA, relB)
}

func compareSegments(a, b string) int {
	if a == b {
		return 0
	}
	sa := tokenizeVersion(a)
	sb := tokenizeVersion(b)
	for i := 0; i < len(sa) && i < len(sb); i++ {
		if sa[i] == sb[i] {
			continue
		}
		na, errA := strconv.Atoi(sa[i])
		nb, errB := strconv.Atoi(sb[i])
		if errA == nil && errB == nil {
			if na < nb {
				return -1
			}
			if na > nb {
				return 1
			}
			continue
		}
		if sa[i] < sb[i] {
			return -1
		}
		return 1
	}
	if len(sa) < len(sb) {
		return -1
	}
	if len(sa) > len(sb) {
		return 1
	}
	return 0
}

func tokenizeVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	})
}
