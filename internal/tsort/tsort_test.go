package tsort

import (
	"cmp"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// --- Direct ports of Ruby's test/test_tsort.rb ---

func TestRubyTSortDAG(t *testing.T) {
	g := MapGraph[int]{1: {2, 3}, 2: {3}, 3: {}}
	got, err := Sort[int](g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{3, 2, 1}
	if !slices.Equal(got, want) {
		t.Fatalf("Sort = %v, want %v", got, want)
	}

	sccs := SCCs[int](g)
	wantSCC := [][]int{{3}, {2}, {1}}
	if !equalNested(sccs, wantSCC) {
		t.Fatalf("SCCs = %v, want %v", sccs, wantSCC)
	}
}

func TestRubyTSortCycle(t *testing.T) {
	g := MapGraph[int]{1: {2}, 2: {3, 4}, 3: {2}, 4: {}}
	sccs := SCCs[int](g)
	for i := range sccs {
		slices.Sort(sccs[i])
	}
	wantSCC := [][]int{{4}, {2, 3}, {1}}
	if !equalNested(sccs, wantSCC) {
		t.Fatalf("SCCs = %v, want %v", sccs, wantSCC)
	}

	_, err := Sort[int](g)
	var cycErr *CyclicError[int]
	if !errors.As(err, &cycErr) {
		t.Fatalf("expected CyclicError, got %v", err)
	}
	if len(cycErr.CyclePath) < 3 || cycErr.CyclePath[0] != cycErr.CyclePath[len(cycErr.CyclePath)-1] {
		t.Fatalf("expected valid closed cycle path, got %v", cycErr.CyclePath)
	}
}

func TestRubyTSortArrayGraph(t *testing.T) {
	arrayGraph := func(adj [][]int) FuncGraph[int] {
		return FuncGraph[int]{
			Nodes: func(yield func(int) bool) {
				for i := range adj {
					if !yield(i) {
						return
					}
				}
			},
			Children: func(n int, yield func(int) bool) {
				for _, c := range adj[n] {
					if !yield(c) {
						return
					}
				}
			},
		}
	}

	sccs := SCCs[int](arrayGraph([][]int{{1}, {0}, {0}, {2}}))
	for i := range sccs {
		slices.Sort(sccs[i])
	}
	if !equalNested(sccs, [][]int{{0, 1}, {2}, {3}}) {
		t.Fatalf("SCCs = %v", sccs)
	}

	sccs2 := SCCs[int](arrayGraph([][]int{{}, {0}}))
	if !equalNested(sccs2, [][]int{{0}, {1}}) {
		t.Fatalf("SCCs2 = %v", sccs2)
	}
}

func TestRubyTSortDiamondAndEachSCCFrom(t *testing.T) {
	g := MapGraph[int]{1: {2, 3}, 2: {4}, 3: {2, 4}, 4: {}}
	got, err := Sort[int](g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(got, []int{4, 2, 3, 1}) {
		t.Fatalf("Sort = %v, want [4 2 3 1]", got)
	}

	cyc := MapGraph[int]{1: {2}, 2: {3, 4}, 3: {2}, 4: {}}
	var from1 [][]int
	EachSCCFrom(1, cyc.EachChild, func(c []int) bool {
		slices.Sort(c)
		from1 = append(from1, c)
		return true
	})
	if !equalNested(from1, [][]int{{4}, {2, 3}, {1}}) {
		t.Fatalf("EachSCCFrom(1) = %v", from1)
	}
}

// --- Additional package-manager edge cases ---

func TestSelfLoopDetected(t *testing.T) {
	g := MapGraph[string]{
		"a": {"a"},
		"b": {"a"},
	}
	_, err := Sort[string](g)
	var cycErr *CyclicError[string]
	if !errors.As(err, &cycErr) {
		t.Fatalf("expected CyclicError on self-loop, got %v", err)
	}
	if !slices.Equal(cycErr.CyclePath, []string{"a", "a"}) {
		t.Fatalf("CyclePath = %v, want [a a]", cycErr.CyclePath)
	}

	dag := Condense[string](g)
	if len(dag.Cycles) != 1 || !slices.Equal(dag.Cycles[0], []string{"a"}) {
		t.Fatalf("dag.Cycles = %v, want [[a]]", dag.Cycles)
	}
}

func TestSortCondensedDeterministicPriorityAndCycleBreak(t *testing.T) {
	// Diamond + cycle: app -> {freetype, zlib}, freetype <-> harfbuzz, harfbuzz -> glib, glib -> glibc, zlib -> glibc
	g := MapGraph[string]{
		"app":      {"freetype", "zlib"},
		"freetype": {"harfbuzz"},
		"harfbuzz": {"freetype", "glib"},
		"glib":     {"glibc"},
		"zlib":     {"glibc"},
		"glibc":    {},
	}

	prio := map[string]int{"glibc": 0, "zlib": 1}
	less := func(a, b string) bool {
		pa, okA := prio[a]
		if !okA {
			pa = 10
		}
		pb, okB := prio[b]
		if !okB {
			pb = 10
		}
		if pa != pb {
			return pa < pb
		}
		return a < b
	}

	// Strict mode must fail with cycle witness.
	_, _, err := SortCondensed[string](g, less, true)
	if err == nil || !strings.Contains(err.Error(), "freetype") || !strings.Contains(err.Error(), "harfbuzz") {
		t.Fatalf("expected strict cycle error mentioning freetype/harfbuzz, got %v", err)
	}

	// Non-strict mode groups the SCC cleanly: glibc -> zlib -> glib -> [freetype, harfbuzz] -> app.
	order, cycles, err := SortCondensed[string](g, less, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle SCC, got %v", cycles)
	}
	want := []string{"glibc", "zlib", "glib", "freetype", "harfbuzz", "app"}
	if !slices.Equal(order, want) {
		t.Fatalf("SortCondensed = %v, want %v", order, want)
	}
}

func TestDeterminismAcrossShuffledInsertionOrder(t *testing.T) {
	edges := [][2]string{
		{"sway", "wlroots"},
		{"sway", "json-c"},
		{"sway", "pango"},
		{"wlroots", "wayland"},
		{"wlroots", "mesa"},
		{"wlroots", "libinput"},
		{"mesa", "llvm"},
		{"mesa", "libdrm"},
		{"pango", "cairo"},
		{"pango", "harfbuzz"},
		{"cairo", "libpng"},
		{"cairo", "zlib"},
		{"libpng", "zlib"},
		{"llvm", "zlib"},
	}

	var baseline []string
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 50; iter++ {
		perm := slices.Clone(edges)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		g := NewAdjGraph[string]()
		for _, e := range perm {
			g.AddEdge(e[0], e[1])
		}
		order, _, err := SortCondensed[string](g, cmp.Less[string], true)
		if err != nil {
			t.Fatalf("iter %d unexpected error: %v", iter, err)
		}
		if baseline == nil {
			baseline = order
		} else if !slices.Equal(order, baseline) {
			t.Fatalf("non-deterministic output on iter %d: got %v, baseline %v", iter, order, baseline)
		}
	}
}

func TestWavesLongestPathAndCycleGrouping(t *testing.T) {
	// Wave 0: base
	// Wave 1: a (depends on base), b (depends on base)
	// Wave 2: c1 <-> c2 (depends on a)
	// Wave 3: top (depends on c1, b)
	g := MapGraph[string]{
		"base": {},
		"a":    {"base"},
		"b":    {"base"},
		"c1":   {"c2", "a"},
		"c2":   {"c1"},
		"top":  {"c1", "b"},
	}
	waves, cycles := Waves[string](g, cmp.Less[string])
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %v", cycles)
	}
	want := [][]string{
		{"base"},
		{"a", "b"},
		{"c1", "c2"},
		{"top"},
	}
	if !equalNested(waves, want) {
		t.Fatalf("Waves = %v, want %v", waves, want)
	}
}

func TestReverseGraph(t *testing.T) {
	g := MapGraph[string]{
		"app": {"mid"},
		"mid": {"lib"},
		"lib": {},
	}
	rev := Reverse[string](g)
	order, _, err := SortCondensed[string](rev, cmp.Less[string], true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// In reversed graph, lib -> mid -> app, so children-first yields app, mid, lib (safe removal order!).
	want := []string{"app", "mid", "lib"}
	if !slices.Equal(order, want) {
		t.Fatalf("Reverse removal order = %v, want %v", order, want)
	}
}

func TestDeepChainNoStackOverflow(t *testing.T) {
	const n = 20000
	g := NewAdjGraph[int]()
	for i := 0; i < n; i++ {
		if i == 0 {
			g.AddNode(0)
		} else {
			g.AddEdge(i, i-1)
		}
	}
	order, err := Sort[int](g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != n || order[0] != 0 || order[n-1] != n-1 {
		t.Fatalf("unexpected order length=%d first=%d last=%d", len(order), order[0], order[len(order)-1])
	}
}

func BenchmarkSortCondensed2500Nodes(b *testing.B) {
	const n = 2500
	g := NewAdjGraph[string]()
	rng := rand.New(rand.NewSource(7))
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("pkg-%04d", i)
		g.AddNode(names[i])
		// Acyclic edges strictly toward lower indices (average 4 deps per pkg).
		for k := 0; k < 4 && i > 0; k++ {
			dep := rng.Intn(i)
			g.AddEdge(names[i], names[dep])
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		order, _, err := SortCondensed[string](g, cmp.Less[string], true)
		if err != nil || len(order) != n {
			b.Fatalf("unexpected result: len=%d err=%v", len(order), err)
		}
	}
}

func equalNested[T comparable](a, b [][]T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !slices.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
