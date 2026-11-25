// Package depgraph implements kpm's unified package dependency graph engine
// on top of internal/tsort (Tarjan SCC + Kahn condensation DAG).
//
// It powers install, update/upgrade, remove (with optional --cascade),
// autoremove, why-installed explanations, reverse-dependency queries,
// ASCII tree rendering, and Graphviz DOT export.
package depgraph

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kryx-linux/kpm/internal/tsort"
)

// DepsFunc returns the direct dependency package names for pkg.
type DepsFunc func(pkg string) []string

// Options controls cycle policy and traversal behavior.
type Options struct {
	StrictCycles     bool // If true, return *tsort.CyclicError on any dependency cycle
	IncludeInstalled bool // For ResolveInstall: expand & include already-installed deps
}

// CycleInfo describes a detected dependency cycle SCC and a minimal witness path.
type CycleInfo struct {
	Members []string `json:"members"`
	Path    []string `json:"path"`
}

func (c CycleInfo) String() string {
	if len(c.Path) > 0 {
		return strings.Join(c.Path, " -> ")
	}
	return strings.Join(c.Members, ", ")
}

// InstallPlan is the resolved, topologically ordered plan for `kpm install`.
type InstallPlan struct {
	Order    []string      `json:"order"`
	Explicit []string      `json:"explicit"`
	NewDeps  []string      `json:"new_deps"`
	Waves    [][]string    `json:"waves,omitempty"`
	Cycles   []CycleInfo   `json:"cycles,omitempty"`
}

// UpgradeStep is one ordered step in a unified `kpm update`/`upgrade` transaction.
type UpgradeStep struct {
	Name     string `json:"name"`
	IsNewDep bool   `json:"is_new_dep"`
}

// UpgradePlan is the resolved, topologically ordered plan for `kpm update`.
type UpgradePlan struct {
	Steps    []UpgradeStep `json:"steps"`
	NewDeps  []string      `json:"new_deps"`
	Upgrades []string      `json:"upgrades"`
	Waves    [][]string    `json:"waves,omitempty"`
	Cycles   []CycleInfo   `json:"cycles,omitempty"`
}

// RemovePlan is the resolved, reverse-topologically ordered plan for `kpm remove`
// and `kpm autoremove` (dependents strictly before dependencies).
type RemovePlan struct {
	Order    []string            `json:"order"`
	Blockers map[string][]string `json:"blockers,omitempty"` // pkg -> installed dependents blocking removal
	Waves    [][]string          `json:"waves,omitempty"`
	Cycles   []CycleInfo         `json:"cycles,omitempty"`
}

// WhyResult explains why an installed package is present on the system.
type WhyResult struct {
	Target     string     `json:"target"`
	Installed  bool       `json:"installed"`
	IsExplicit bool       `json:"is_explicit"`
	IsOrphan   bool       `json:"is_orphan"`
	Paths      [][]string `json:"paths,omitempty"` // Shortest chains from world root -> ... -> Target
}

var corePriority = map[string]int{
	"filesystem":        0,
	"glibc":             1,
	"zlib":              2,
	"bzip2":             2,
	"xz":                2,
	"zstd":              2,
	"attr":              2,
	"acl":               2,
	"libcap":            2,
	"libcap-ng":         2,
	"libxcrypt":         2,
	"libpcre2":          2,
	"libgmp":            2,
	"libmpfr":           2,
	"libmd":             2,
	"libbsd":            2,
	"libtirpc":          2,
	"libnsl":            2,
	"openssl":           2,
	"ca-certificates":   2,
	"linux-pam":         2,
	"ncurses":           2,
	"readline":          2,
	"dash":              2,
	"bash":              2,
	"coreutils":         2,
	"eudev":             2,
	"util-linux":        2,
	"sysvinit":          2,
	"rc":                2,
	"start-stop-daemon": 2,
	"kpm":               3,
	"signify":           3,
}

// ComparePkgPriority orders foundational core packages ahead of ordinary
// packages when topological dependencies allow either order, breaking all
// remaining ties lexicographically for 100% deterministic plans.
func ComparePkgPriority(a, b string) bool {
	pa, okA := corePriority[a]
	if !okA {
		pa = 10
	}
	pb, okB := corePriority[b]
	if !okB {
		pb = 10
	}
	if pa != pb {
		return pa < pb
	}
	return a < b
}

// CompareRemovePriority breaks ties during package removal in reverse
// priority order (higher-level applications first, foundational core last).
func CompareRemovePriority(a, b string) bool {
	pa, okA := corePriority[a]
	if !okA {
		pa = 10
	}
	pb, okB := corePriority[b]
	if !okB {
		pb = 10
	}
	if pa != pb {
		return pa > pb
	}
	return a < b
}

func extractCycles(g *tsort.AdjGraph[string]) []CycleInfo {
	dag := tsort.Condense[string](g)
	if len(dag.Cycles) == 0 {
		return nil
	}
	out := make([]CycleInfo, len(dag.Cycles))
	for i := range dag.Cycles {
		members := slices.Clone(dag.Cycles[i])
		sort.Strings(members)
		out[i] = CycleInfo{
			Members: members,
			Path:    slices.Clone(dag.CyclePaths[i]),
		}
	}
	return out
}

// ResolveInstall computes a deterministic, dependencies-first installation
// order for targets (skipping already-installed non-target dependencies unless
// opts.IncludeInstalled is set).
func ResolveInstall(targets []string, installed map[string]bool, depsFn DepsFunc, opts Options) (InstallPlan, error) {
	targetSet := make(map[string]bool, len(targets))
	explicit := make([]string, 0, len(targets))
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" || targetSet[t] {
			continue
		}
		targetSet[t] = true
		explicit = append(explicit, t)
	}

	g := tsort.NewAdjGraph[string]()
	visited := make(map[string]bool)

	var expand func(name string)
	expand = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		g.AddNode(name)
		if depsFn == nil {
			return
		}
		for _, dep := range depsFn(name) {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if !targetSet[dep] && installed[dep] && !opts.IncludeInstalled {
				continue
			}
			g.AddEdge(name, dep)
			expand(dep)
		}
	}

	for _, t := range explicit {
		expand(t)
	}

	order, _, err := tsort.SortCondensed[string](g, ComparePkgPriority, opts.StrictCycles)
	if err != nil {
		return InstallPlan{}, err
	}
	waves, _ := tsort.Waves[string](g, ComparePkgPriority)
	cycles := extractCycles(g)

	var newDeps []string
	for _, name := range order {
		if !targetSet[name] {
			newDeps = append(newDeps, name)
		}
	}

	return InstallPlan{
		Order:    order,
		Explicit: explicit,
		NewDeps:  newDeps,
		Waves:    waves,
		Cycles:   cycles,
	}, nil
}

// ResolveUpgrade computes ONE unified topological transaction for upgrading
// toUpgrade along with any newly required uninstalled dependencies.
//
// Crucially, it bridges transitive dependencies through unchanged installed
// packages (e.g. if upgraded `app` -> unchanged `midlib` -> upgraded `lib`,
// `lib` is still guaranteed to upgrade strictly before `app`).
func ResolveUpgrade(toUpgrade []string, installed map[string]bool, depsFn DepsFunc, opts Options) (UpgradePlan, error) {
	upgradeSet := make(map[string]bool, len(toUpgrade))
	var uniqueUpgrades []string
	for _, u := range toUpgrade {
		u = strings.TrimSpace(u)
		if u == "" || upgradeSet[u] {
			continue
		}
		upgradeSet[u] = true
		uniqueUpgrades = append(uniqueUpgrades, u)
	}
	if len(uniqueUpgrades) == 0 {
		return UpgradePlan{}, nil
	}

	// 1. Discover all newly introduced uninstalled runtime dependencies.
	newDepSet := make(map[string]bool)
	seenNew := make(map[string]bool)
	var findNewDeps func(name string)
	findNewDeps = func(name string) {
		if seenNew[name] || depsFn == nil {
			return
		}
		seenNew[name] = true
		for _, dep := range depsFn(name) {
			dep = strings.TrimSpace(dep)
			if dep == "" || upgradeSet[dep] || installed[dep] {
				continue
			}
			newDepSet[dep] = true
			findNewDeps(dep)
		}
	}
	for _, u := range uniqueUpgrades {
		findNewDeps(u)
	}

	// 2. Build unified transaction graph over txSet = upgradeSet ∪ newDepSet,
	//    bridging transitive paths through unchanged installed packages.
	txSet := make(map[string]bool, len(upgradeSet)+len(newDepSet))
	txNodes := make([]string, 0, len(upgradeSet)+len(newDepSet))
	for _, u := range uniqueUpgrades {
		txSet[u] = true
		txNodes = append(txNodes, u)
	}
	for d := range newDepSet {
		txSet[d] = true
		txNodes = append(txNodes, d)
	}
	sort.Strings(txNodes)

	g := tsort.NewAdjGraph[string]()
	for _, n := range txNodes {
		g.AddNode(n)
	}

	if depsFn != nil {
		for _, src := range txNodes {
			// BFS through unchanged installed packages until reaching another node in txSet.
			visited := map[string]bool{src: true}
			queue := []string{src}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				for _, dep := range depsFn(cur) {
					dep = strings.TrimSpace(dep)
					if dep == "" || dep == src {
						if dep == src && cur == src {
							g.AddEdge(src, src)
						}
						continue
					}
					if txSet[dep] {
						g.AddEdge(src, dep)
						continue // dep will contribute its own outgoing edges
					}
					if installed[dep] && !visited[dep] {
						visited[dep] = true
						queue = append(queue, dep)
					}
				}
			}
		}
	}

	order, _, err := tsort.SortCondensed[string](g, ComparePkgPriority, opts.StrictCycles)
	if err != nil {
		return UpgradePlan{}, err
	}
	waves, _ := tsort.Waves[string](g, ComparePkgPriority)
	cycles := extractCycles(g)

	steps := make([]UpgradeStep, 0, len(order))
	var orderedNewDeps, orderedUpgrades []string
	for _, name := range order {
		isNew := newDepSet[name]
		steps = append(steps, UpgradeStep{Name: name, IsNewDep: isNew})
		if isNew {
			orderedNewDeps = append(orderedNewDeps, name)
		} else {
			orderedUpgrades = append(orderedUpgrades, name)
		}
	}

	return UpgradePlan{
		Steps:    steps,
		NewDeps:  orderedNewDeps,
		Upgrades: orderedUpgrades,
		Waves:    waves,
		Cycles:   cycles,
	}, nil
}

// ReverseDeps builds a deterministic map of pkg -> sorted direct installed
// dependents for all installed packages.
func ReverseDeps(installed map[string]bool, depsFn DepsFunc) map[string][]string {
	rev := make(map[string][]string)
	if depsFn == nil || len(installed) == 0 {
		return rev
	}
	names := sortedKeys(installed)
	for _, pkg := range names {
		seenDep := make(map[string]bool)
		for _, dep := range depsFn(pkg) {
			dep = strings.TrimSpace(dep)
			if dep == "" || dep == pkg || seenDep[dep] {
				continue
			}
			seenDep[dep] = true
			rev[dep] = append(rev[dep], pkg)
		}
	}
	for k := range rev {
		sort.Strings(rev[k])
	}
	return rev
}

// ResolveRemove computes a safe removal order (dependents strictly before
// dependencies) and detects any installed reverse-dependency blockers.
//
//   - If cascade is false: Blockers maps each target to installed dependents
//     outside the removal set.
//   - If cascade is true: expands targets to include all transitive installed
//     dependents so the entire dependent cone is removed cleanly in order.
func ResolveRemove(targets []string, installed map[string]bool, depsFn DepsFunc, cascade bool) (RemovePlan, error) {
	revMap := ReverseDeps(installed, depsFn)

	toRemove := make(map[string]bool, len(targets))
	var seed []string
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" || toRemove[t] {
			continue
		}
		toRemove[t] = true
		seed = append(seed, t)
	}

	if cascade {
		queue := slices.Clone(seed)
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, dependent := range revMap[cur] {
				if installed[dependent] && !toRemove[dependent] {
					toRemove[dependent] = true
					queue = append(queue, dependent)
				}
			}
		}
	}

	var blockers map[string][]string
	if !cascade {
		for _, t := range seed {
			if !installed[t] {
				continue
			}
			var blockedBy []string
			for _, dep := range revMap[t] {
				if installed[dep] && !toRemove[dep] {
					blockedBy = append(blockedBy, dep)
				}
			}
			if len(blockedBy) > 0 {
				if blockers == nil {
					blockers = make(map[string][]string)
				}
				sort.Strings(blockedBy)
				blockers[t] = blockedBy
			}
		}
	}

	order, waves, cycles := sortForRemoval(toRemove, installed, depsFn)
	return RemovePlan{
		Order:    order,
		Blockers: blockers,
		Waves:    waves,
		Cycles:   cycles,
	}, nil
}

// ResolveAutoremove finds all installed packages unreachable from world and
// orders them in strict reverse-dependency order (dependents before deps).
func ResolveAutoremove(world map[string]bool, installed map[string]bool, depsFn DepsFunc) (RemovePlan, error) {
	reachable := make(map[string]bool, len(world))
	queue := make([]string, 0, len(world))
	for w := range world {
		if installed[w] && !reachable[w] {
			reachable[w] = true
			queue = append(queue, w)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if depsFn == nil {
			continue
		}
		for _, dep := range depsFn(cur) {
			dep = strings.TrimSpace(dep)
			if dep != "" && installed[dep] && !reachable[dep] {
				reachable[dep] = true
				queue = append(queue, dep)
			}
		}
	}

	orphans := make(map[string]bool)
	for pkg := range installed {
		if !reachable[pkg] {
			orphans[pkg] = true
		}
	}
	if len(orphans) == 0 {
		return RemovePlan{}, nil
	}

	order, waves, cycles := sortForRemoval(orphans, installed, depsFn)
	return RemovePlan{
		Order:  order,
		Waves:  waves,
		Cycles: cycles,
	}, nil
}

// sortForRemoval orders pkgSet so dependents ALWAYS appear before their
// dependencies, bridging transitive paths through any installed packages.
func sortForRemoval(pkgSet map[string]bool, installed map[string]bool, depsFn DepsFunc) ([]string, [][]string, []CycleInfo) {
	nodes := sortedKeys(pkgSet)
	// Build reversed graph: edge dep -> dependent so tsort emits dependents first!
	revGraph := tsort.NewAdjGraph[string]()
	for _, n := range nodes {
		revGraph.AddNode(n)
	}
	if depsFn != nil {
		for _, src := range nodes {
			visited := map[string]bool{src: true}
			queue := []string{src}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				for _, dep := range depsFn(cur) {
					dep = strings.TrimSpace(dep)
					if dep == "" || dep == src {
						continue
					}
					if pkgSet[dep] {
						// src depends on dep -> in removal graph, dep must wait for src!
						revGraph.AddEdge(dep, src)
						continue
					}
					if installed[dep] && !visited[dep] {
						visited[dep] = true
						queue = append(queue, dep)
					}
				}
			}
		}
	}

	order, _, _ := tsort.SortCondensed[string](revGraph, CompareRemovePriority, false)
	waves, _ := tsort.Waves[string](revGraph, CompareRemovePriority)
	cycles := extractCycles(revGraph)
	return order, waves, cycles
}

// Why explains why target is installed by finding shortest dependency paths
// from explicitly installed world packages down to target.
func Why(target string, world map[string]bool, installed map[string]bool, depsFn DepsFunc) WhyResult {
	res := WhyResult{
		Target:     target,
		Installed:  installed[target],
		IsExplicit: world[target],
	}
	if !res.Installed {
		return res
	}

	revMap := ReverseDeps(installed, depsFn)

	// BFS backwards from target through reverse deps to find shortest paths to world roots.
	dist := map[string]int{target: 0}
	child := make(map[string]string) // next hop toward target
	queue := []string{target}
	var worldHits []string

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dependent := range revMap[cur] {
			if !installed[dependent] {
				continue
			}
			if _, seen := dist[dependent]; seen {
				continue
			}
			dist[dependent] = dist[cur] + 1
			child[dependent] = cur
			if world[dependent] {
				worldHits = append(worldHits, dependent)
				continue // don't need to expand past an explicit world root
			}
			queue = append(queue, dependent)
		}
	}

	sort.Slice(worldHits, func(i, j int) bool {
		if dist[worldHits[i]] != dist[worldHits[j]] {
			return dist[worldHits[i]] < dist[worldHits[j]]
		}
		return worldHits[i] < worldHits[j]
	})

	for _, root := range worldHits {
		path := []string{root}
		for cur := root; cur != target; cur = child[cur] {
			path = append(path, child[cur])
		}
		res.Paths = append(res.Paths, path)
	}

	if !res.IsExplicit && len(res.Paths) == 0 {
		res.IsOrphan = true
	}
	return res
}

// BuildSubgraph builds the reachable dependency graph starting from roots.
// When reverse is true, edges follow reverse dependencies across universe.
func BuildSubgraph(roots []string, depsFn DepsFunc, reverse bool, universe []string) *tsort.AdjGraph[string] {
	var nextFn DepsFunc
	if reverse {
		uSet := make(map[string]bool, len(universe))
		for _, u := range universe {
			uSet[u] = true
		}
		revMap := ReverseDeps(uSet, depsFn)
		nextFn = func(pkg string) []string {
			return revMap[pkg]
		}
	} else {
		nextFn = depsFn
	}

	g := tsort.NewAdjGraph[string]()
	visited := make(map[string]bool)
	var walk func(name string)
	walk = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		g.AddNode(name)
		if nextFn == nil {
			return
		}
		for _, dep := range nextFn(name) {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			g.AddEdge(name, dep)
			walk(dep)
		}
	}
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r != "" {
			walk(r)
		}
	}
	return g
}

// RenderTree formats an ASCII dependency tree rooted at root with cycle and
// duplicate subtree annotations. maxDepth <= 0 means unlimited depth.
func RenderTree(root string, depsFn DepsFunc, maxDepth int) string {
	var sb strings.Builder
	sb.WriteString(root)
	sb.WriteByte('\n')

	expanded := map[string]bool{root: true}
	onPath := map[string]bool{root: true}

	var walk func(node, prefix string, depth int)
	walk = func(node, prefix string, depth int) {
		if depsFn == nil || (maxDepth > 0 && depth >= maxDepth) {
			return
		}
		kids := depsFn(node)
		for i, kid := range kids {
			kid = strings.TrimSpace(kid)
			if kid == "" {
				continue
			}
			last := i == len(kids)-1
			branch := "├── "
			childPrefix := prefix + "│   "
			if last {
				branch = "└── "
				childPrefix = prefix + "    "
			}
			if onPath[kid] {
				sb.WriteString(prefix + branch + kid + " (cycle)\n")
				continue
			}
			if expanded[kid] {
				sb.WriteString(prefix + branch + kid + " (*)\n")
				continue
			}
			sb.WriteString(prefix + branch + kid + "\n")
			expanded[kid] = true
			onPath[kid] = true
			walk(kid, childPrefix, depth+1)
			delete(onPath, kid)
		}
	}

	walk(root, "", 0)
	return sb.String()
}

// RenderDOT exports g as a deterministic Graphviz DOT digraph.
func RenderDOT(g *tsort.AdjGraph[string], title string) string {
	if strings.TrimSpace(title) == "" {
		title = "kpm_deps"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "digraph %q {\n", title)
	sb.WriteString("  rankdir=LR;\n")
	sb.WriteString("  node [shape=box];\n")
	nodes := g.Nodes()
	sort.Strings(nodes)
	for _, n := range nodes {
		kids := slices.Clone(g.Children(n))
		sort.Strings(kids)
		if len(kids) == 0 {
			fmt.Fprintf(&sb, "  %q;\n", n)
			continue
		}
		for _, k := range kids {
			fmt.Fprintf(&sb, "  %q -> %q;\n", n, k)
		}
	}
	sb.WriteString("}\n")
	return sb.String()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
