package depgraph

import (
	"slices"
	"strings"
	"testing"
)

func mapDeps(m map[string][]string) DepsFunc {
	return func(pkg string) []string {
		return m[pkg]
	}
}

func TestResolveInstallPriorityAndSkipInstalled(t *testing.T) {
	deps := map[string][]string{
		"sway":    {"wlroots", "pango"},
		"wlroots": {"mesa", "glibc"},
		"pango":   {"cairo"},
		"cairo":   {"zlib"},
		"mesa":    {"zlib"},
		"zlib":    {"glibc"},
		"glibc":   {"filesystem"},
	}
	installed := map[string]bool{
		"filesystem": true,
		"glibc":      true,
	}

	plan, err := ResolveInstall([]string{"sway"}, installed, mapDeps(deps), Options{StrictCycles: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// filesystem and glibc are already installed and must not appear.
	if slices.Contains(plan.Order, "glibc") || slices.Contains(plan.Order, "filesystem") {
		t.Fatalf("already installed deps should be skipped, got %v", plan.Order)
	}
	// zlib must come before mesa and cairo; mesa before wlroots; cairo before pango; both before sway.
	pos := indexMap(plan.Order)
	assertBefore(t, pos, "zlib", "mesa")
	assertBefore(t, pos, "zlib", "cairo")
	assertBefore(t, pos, "mesa", "wlroots")
	assertBefore(t, pos, "cairo", "pango")
	assertBefore(t, pos, "wlroots", "sway")
	assertBefore(t, pos, "pango", "sway")
}

func TestResolveUpgradeTransitiveThroughUnchangedInstalled(t *testing.T) {
	// Regression test for alphabetical kpm update bug:
	// "app" < "libbase" alphabetically, and "app" depends on unchanged "midlib" which depends on "libbase".
	// Plus "app" introduces a brand new uninstalled dependency "newdep" which depends on "libbase".
	deps := map[string][]string{
		"app":     {"midlib", "newdep"},
		"midlib":  {"libbase"},
		"newdep":  {"libbase"},
		"libbase": {"glibc"},
	}
	installed := map[string]bool{
		"app":     true,
		"midlib":  true,
		"libbase": true,
		"glibc":   true,
	}

	plan, err := ResolveUpgrade([]string{"app", "libbase"}, installed, mapDeps(deps), Options{StrictCycles: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	names := make([]string, len(plan.Steps))
	for i, s := range plan.Steps {
		names[i] = s.Name
	}
	want := []string{"libbase", "newdep", "app"}
	if !slices.Equal(names, want) {
		t.Fatalf("ResolveUpgrade order = %v, want %v", names, want)
	}
	if !plan.Steps[1].IsNewDep || plan.Steps[0].IsNewDep || plan.Steps[2].IsNewDep {
		t.Fatalf("unexpected IsNewDep flags: %+v", plan.Steps)
	}
}

func TestResolveRemoveOrderAndBlockers(t *testing.T) {
	deps := map[string][]string{
		"top":    {"mid"},
		"mid":    {"libfoo"},
		"libfoo": {"glibc"},
	}
	installed := map[string]bool{
		"top":    true,
		"mid":    true,
		"libfoo": true,
		"glibc":  true,
	}

	// 1. Removing libfoo alone must be blocked by mid.
	plan, err := ResolveRemove([]string{"libfoo"}, installed, mapDeps(deps), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(plan.Blockers["libfoo"], []string{"mid"}) {
		t.Fatalf("expected blocker [mid] for libfoo, got %v", plan.Blockers)
	}

	// 2. Removing in wrong CLI order (libfoo mid top) must reorder to top -> mid -> libfoo!
	plan2, err := ResolveRemove([]string{"libfoo", "mid", "top"}, installed, mapDeps(deps), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan2.Blockers) != 0 {
		t.Fatalf("expected zero blockers, got %v", plan2.Blockers)
	}
	if !slices.Equal(plan2.Order, []string{"top", "mid", "libfoo"}) {
		t.Fatalf("Remove order = %v, want [top mid libfoo]", plan2.Order)
	}

	// 3. Cascade removal of libfoo must expand and remove top -> mid -> libfoo!
	plan3, err := ResolveRemove([]string{"libfoo"}, installed, mapDeps(deps), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(plan3.Order, []string{"top", "mid", "libfoo"}) {
		t.Fatalf("Cascade remove order = %v, want [top mid libfoo]", plan3.Order)
	}
}

func TestResolveAutoremoveReverseTopoOrder(t *testing.T) {
	// Regression test for alphabetical cmdAutoremove bug:
	// "aaa-lib" < "zzz-app" alphabetically, but zzz-app depends on aaa-lib!
	// Both are orphans (unreachable from world={"keep"}).
	deps := map[string][]string{
		"keep":    {"glibc"},
		"zzz-app": {"aaa-lib"},
		"aaa-lib": {"glibc"},
	}
	installed := map[string]bool{
		"keep":    true,
		"glibc":   true,
		"zzz-app": true,
		"aaa-lib": true,
	}
	world := map[string]bool{"keep": true}

	plan, err := ResolveAutoremove(world, installed, mapDeps(deps))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"zzz-app", "aaa-lib"}
	if !slices.Equal(plan.Order, want) {
		t.Fatalf("Autoremove order = %v, want %v", plan.Order, want)
	}
}

func TestWhyShortestPathsFromWorld(t *testing.T) {
	deps := map[string][]string{
		"sway":    {"wlroots"},
		"wlroots": {"mesa"},
		"mesa":    {"zlib"},
		"curl":    {"zlib"},
		"orphan":  {"zlib"},
	}
	installed := map[string]bool{
		"sway": true, "wlroots": true, "mesa": true, "curl": true, "zlib": true, "orphan": true,
	}
	world := map[string]bool{"sway": true, "curl": true}

	why := Why("zlib", world, installed, mapDeps(deps))
	if !why.Installed || why.IsExplicit || why.IsOrphan {
		t.Fatalf("unexpected why flags: %+v", why)
	}
	if len(why.Paths) != 2 {
		t.Fatalf("expected 2 paths to zlib, got %v", why.Paths)
	}
	if !slices.Equal(why.Paths[0], []string{"curl", "zlib"}) {
		t.Fatalf("expected shortest path [curl zlib], got %v", why.Paths[0])
	}
	if !slices.Equal(why.Paths[1], []string{"sway", "wlroots", "mesa", "zlib"}) {
		t.Fatalf("expected second path [sway wlroots mesa zlib], got %v", why.Paths[1])
	}

	whyOrphan := Why("orphan", world, installed, mapDeps(deps))
	if !whyOrphan.IsOrphan {
		t.Fatalf("expected orphan=true, got %+v", whyOrphan)
	}
}

func TestRenderTreeAndDOT(t *testing.T) {
	deps := map[string][]string{
		"a": {"b", "c"},
		"b": {"c", "a"}, // cycle back to a, shared dep c
		"c": {},
	}
	tree := RenderTree("a", mapDeps(deps), 0)
	if !strings.Contains(tree, "(cycle)") || !strings.Contains(tree, "(*)") {
		t.Fatalf("expected cycle and duplicate annotations in tree:\n%s", tree)
	}

	g := BuildSubgraph([]string{"a"}, mapDeps(deps), false, nil)
	dot := RenderDOT(g, "test")
	if !strings.Contains(dot, `"a" -> "b"`) || !strings.Contains(dot, `"b" -> "a"`) {
		t.Fatalf("unexpected DOT output:\n%s", dot)
	}
}

func indexMap(order []string) map[string]int {
	m := make(map[string]int, len(order))
	for i, s := range order {
		m[s] = i
	}
	return m
}

func assertBefore(t *testing.T, pos map[string]int, a, b string) {
	t.Helper()
	pa, okA := pos[a]
	pb, okB := pos[b]
	if !okA || !okB {
		t.Fatalf("missing node in order: %s(%v) %s(%v)", a, okA, b, okB)
	}
	if pa >= pb {
		t.Fatalf("expected %s (pos %d) before %s (pos %d)", a, pa, b, pb)
	}
}
