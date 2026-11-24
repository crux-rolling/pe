package tree

import (
	"github.com/kryx-linux/kpm/internal/portscli/kpmcompat"
)

// DiffEntry describes a version difference between a local port and an installed package.
type DiffEntry struct {
	Collection string
	Name       string
	PortVer    string
	Installed  string
	Compare    int // >0 if port is newer, <0 if port is older, 0 if segment-equal but string-different
}

// Diff compares installed packages from kpm's package database against local ports,
// honoring kpm.ini's collection priority order and exact version comparison semantics.
func Diff(db *kpmcompat.DB, localPorts []Port, collections []string) []DiffEntry {
	if db == nil {
		return nil
	}
	portMap := PriorityMap(localPorts, collections)

	var diffs []DiffEntry
	for _, name := range db.SortedNames() {
		instVer := db.Packages[name].Version
		p, ok := portMap[name]
		if !ok {
			continue
		}
		portVer := p.FullVersion()
		if portVer == "" || portVer == instVer {
			continue
		}
		col := p.Collection
		if col == "" {
			col = "port"
		}
		diffs = append(diffs, DiffEntry{
			Collection: col,
			Name:       name,
			PortVer:    portVer,
			Installed:  instVer,
			Compare:    kpmcompat.CompareFullVersion(portVer, instVer),
		})
	}
	return diffs
}
