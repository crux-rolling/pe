package srcmachine

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kryx-linux/kpm/internal/repo"
	"github.com/kryx-linux/kpm/internal/tsort"
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
	MaxWave    int         `json:"max_wave"`
	Truncated  bool        `json:"truncated"`
	Cycles     [][]string  `json:"cycles,omitempty"`
	Include    []MatrixJob `json:"include"`
}

// PlanOptions controls advanced topological wave planning, optional-dep feature policies,
// quarantine skip lists, and incremental index-aware filtering.
type PlanOptions struct {
	Targets     []string
	RepoHint    string
	MaxPorts    int
	ChunkSize   int
	WithDeps    bool                // Expand full transitive dependency closure before wave planning
	WaveFilter  int                 // -1 = all waves; >= 0 = emit only ports in this wave
	Features    map[string][]string // Optional extra_deps enabled per package (from build-features.ini)
	Skip        map[string]bool     // Package names or rel paths to skip (from skip-ports.txt)
	SkipIndexed *repo.Index         // Optional: skip ports whose exact pkgver-pkgrel is already in index
	Arch        string              // Architecture for SkipIndexed lookup (default x86_64)
}

// BuildMatrixPlan resolves target specs into a bounded, topologically ordered
// GitHub Actions matrix plan.
func BuildMatrixPlan(reg *Registry, targets []string, repoHint string, maxPorts, chunkSize int) (MatrixPlan, error) {
	return BuildMatrixPlanWithOptions(reg, PlanOptions{
		Targets:    targets,
		RepoHint:   repoHint,
		MaxPorts:   maxPorts,
		ChunkSize:  chunkSize,
		WaveFilter: -1,
	})
}

// BuildMatrixPlanWithOptions resolves target specs with full transitive closure,
// feature policies, quarantine skip lists, wave filtering, and index-based skipping.
func BuildMatrixPlanWithOptions(reg *Registry, opts PlanOptions) (MatrixPlan, error) {
	maxPorts := opts.MaxPorts
	if maxPorts <= 0 {
		maxPorts = 20
	}
	chunkSize := opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 1
	}
	arch := opts.Arch
	if arch == "" {
		arch = "x86_64"
	}

	roots, err := reg.ExpandTargets(opts.Targets, opts.RepoHint)
	if err != nil {
		return MatrixPlan{}, err
	}

	// Map every known port by package name for fast dependency lookups.
	allByName := make(map[string]Entry, len(reg.Entries))
	for _, e := range reg.Entries {
		if e.Port.Name != "" {
			if _, exists := allByName[e.Port.Name]; !exists {
				allByName[e.Port.Name] = e
			}
		}
	}

	depsOf := func(e Entry) []string {
		base := e.Port.AllBuildDepends()
		if extra := opts.Features[e.Port.Name]; len(extra) > 0 {
			return mergeUniqueDeps(base, extra)
		}
		return base
	}

	isSkipped := func(e Entry) bool {
		if len(opts.Skip) == 0 {
			return false
		}
		return opts.Skip[e.Port.Name] || opts.Skip[e.RelDir]
	}

	// Collect selected entries (optionally expanding full transitive build closure).
	selectedByDir := make(map[string]Entry, len(roots))
	selectedByName := make(map[string]Entry, len(roots))
	var addEntry func(e Entry)
	addEntry = func(e Entry) {
		if isSkipped(e) {
			return
		}
		if _, seen := selectedByDir[e.Port.Dir]; seen {
			return
		}
		selectedByDir[e.Port.Dir] = e
		if e.Port.Name != "" {
			selectedByName[e.Port.Name] = e
		}
		if opts.WithDeps {
			for _, dep := range depsOf(e) {
				if de, ok := allByName[dep]; ok {
					addEntry(de)
				}
			}
		}
	}
	for _, r := range roots {
		addEntry(r)
	}

	// Compute exact longest-path topological waves over the Tarjan SCC
	// condensation DAG via internal/tsort (cycles are grouped cleanly into
	// their SCC wave instead of collapsing to wave 0).
	g := tsort.NewAdjGraph[string]()
	selectedDirs := make([]string, 0, len(selectedByDir))
	for dir := range selectedByDir {
		selectedDirs = append(selectedDirs, dir)
	}
	sort.Slice(selectedDirs, func(i, j int) bool {
		return selectedByDir[selectedDirs[i]].RelDir < selectedByDir[selectedDirs[j]].RelDir
	})
	for _, dir := range selectedDirs {
		e := selectedByDir[dir]
		g.AddNode(dir)
		for _, dep := range depsOf(e) {
			if de, ok := selectedByName[dep]; ok && de.Port.Dir != dir {
				g.AddEdge(dir, de.Port.Dir)
			}
		}
	}

	waves, rawCycles := tsort.Waves[string](g, func(a, b string) bool {
		return selectedByDir[a].RelDir < selectedByDir[b].RelDir
	})

	type weighted struct {
		entry Entry
		wave  int
	}
	items := make([]weighted, 0, len(selectedByDir))
	maxWave := 0
	if len(waves) > 0 {
		maxWave = len(waves) - 1
	}
	for w, dirs := range waves {
		if opts.WaveFilter >= 0 && w != opts.WaveFilter {
			continue
		}
		for _, dir := range dirs {
			e := selectedByDir[dir]
			if opts.SkipIndexed != nil && e.Port.Name != "" {
				if idxEntry, ok := repo.Find(*opts.SkipIndexed, e.Port.Name, arch); ok {
					if idxEntry.Pkgver == e.Port.Version && idxEntry.Pkgrel == e.Port.Release {
						continue
					}
				}
			}
			items = append(items, weighted{entry: e, wave: w})
		}
	}

	var planCycles [][]string
	for _, cyc := range rawCycles {
		names := make([]string, 0, len(cyc))
		for _, dir := range cyc {
			if e, ok := selectedByDir[dir]; ok {
				if e.Port.Name != "" {
					names = append(names, e.Port.Name)
				} else {
					names = append(names, e.RelDir)
				}
			}
		}
		sort.Strings(names)
		planCycles = append(planCycles, names)
	}

	plan := MatrixPlan{
		TotalPorts: len(items),
		MaxWave:    maxWave,
		Cycles:     planCycles,
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

// LoadFeaturesFile parses an INI-style optional-dependency policy file:
//
//	[wlroots]
//	extra_deps = xorg-xwayland xorg-libxcomposite
func LoadFeaturesFile(path string) (map[string][]string, error) {
	out := make(map[string][]string)
	if strings.TrimSpace(path) == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section == "" {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			k := strings.TrimSpace(line[:eq])
			v := strings.TrimSpace(line[eq+1:])
			if k == "extra_deps" || k == "optional" {
				fields := strings.FieldsFunc(v, func(r rune) bool {
					return r == ' ' || r == '\t' || r == ','
				})
				out[section] = mergeUniqueDeps(out[section], fields)
			}
		}
	}
	return out, sc.Err()
}

// LoadSkipFile reads newline-separated package names or collection/port paths to skip.
func LoadSkipFile(path string) (map[string]bool, error) {
	out := make(map[string]bool)
	if strings.TrimSpace(path) == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line != "" {
			out[line] = true
		}
	}
	return out, sc.Err()
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
