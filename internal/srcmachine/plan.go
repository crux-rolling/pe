package srcmachine

import (
	"fmt"
	"sort"
)

// MatrixJob describes a single GitHub Actions matrix job produced by `kryx-src plan`.
type MatrixJob struct {
	ID       string   `json:"id"`
	Wave     int      `json:"wave"`
	Ports    []string `json:"ports"`
	PortArg  string   `json:"port_arg"`
	PkgNames []string `json:"pkg_names"`
}

// MatrixPlan is the JSON structure consumed by GitHub Actions `fromJson(...)`.
type MatrixPlan struct {
	TotalPorts int         `json:"total_ports"`
	Truncated  bool        `json:"truncated"`
	Include    []MatrixJob `json:"include"`
}

// BuildMatrixPlan resolves target specs into a bounded, topologically ordered
// GitHub Actions matrix plan. maxPorts caps total ports per workflow run (default 20
// if <= 0) to stay well within GitHub free-tier public repo concurrency and time limits.
// chunkSize controls how many ports are grouped into each matrix job (default 1).
func BuildMatrixPlan(reg *Registry, targets []string, repoHint string, maxPorts, chunkSize int) (MatrixPlan, error) {
	if maxPorts <= 0 {
		maxPorts = 20
	}
	if chunkSize <= 0 {
		chunkSize = 1
	}

	roots, err := reg.ExpandTargets(targets, repoHint)
	if err != nil {
		return MatrixPlan{}, err
	}

	// Compute topological wave depth among selected roots so dependencies
	// appear in earlier waves than dependents.
	selectedByName := make(map[string]Entry, len(roots))
	selectedByDir := make(map[string]Entry, len(roots))
	for _, e := range roots {
		selectedByDir[e.Port.Dir] = e
		if e.Port.Name != "" {
			selectedByName[e.Port.Name] = e
		}
	}

	memo := make(map[string]int, len(roots))
	visiting := make(map[string]bool)
	var waveOf func(e Entry) int
	waveOf = func(e Entry) int {
		if w, ok := memo[e.Port.Dir]; ok {
			return w
		}
		if visiting[e.Port.Dir] {
			return 0
		}
		visiting[e.Port.Dir] = true
		maxDep := -1
		for _, dep := range e.Port.AllBuildDepends() {
			if de, ok := selectedByName[dep]; ok && de.Port.Dir != e.Port.Dir {
				if dw := waveOf(de); dw > maxDep {
					maxDep = dw
				}
			}
		}
		delete(visiting, e.Port.Dir)
		w := maxDep + 1
		memo[e.Port.Dir] = w
		return w
	}

	type weighted struct {
		entry Entry
		wave  int
	}
	items := make([]weighted, 0, len(roots))
	for _, e := range roots {
		items = append(items, weighted{entry: e, wave: waveOf(e)})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].wave != items[j].wave {
			return items[i].wave < items[j].wave
		}
		return items[i].entry.RelDir < items[j].entry.RelDir
	})

	plan := MatrixPlan{
		TotalPorts: len(items),
	}
	if len(items) > maxPorts {
		items = items[:maxPorts]
		plan.Truncated = true
	}

	// Group items within the same wave into chunks of chunkSize.
	jobNum := 0
	for i := 0; i < len(items); {
		wave := items[i].wave
		var batch []Entry
		for i < len(items) && items[i].wave == wave && len(batch) < chunkSize {
			batch = append(batch, items[i].entry)
			i++
		}
		jobNum++
		portArgs := make([]string, 0, len(batch))
		pkgNames := make([]string, 0, len(batch))
		for _, b := range batch {
			if reg.IsSinglePort {
				portArgs = append(portArgs, b.Port.Name)
			} else {
				portArgs = append(portArgs, b.RelDir)
			}
			pkgNames = append(pkgNames, b.Port.Name)
		}
		id := portArgs[0]
		if len(portArgs) > 1 {
			id = fmt.Sprintf("w%d-chunk-%d", wave, jobNum)
		}
		plan.Include = append(plan.Include, MatrixJob{
			ID:       id,
			Wave:     wave,
			Ports:    portArgs,
			PortArg:  joinSpaces(portArgs),
			PkgNames: pkgNames,
		})
	}
	return plan, nil
}

func joinSpaces(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}
