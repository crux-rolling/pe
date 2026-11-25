// Package tsort implements topological sorting, Tarjan's strongly connected
// components (SCC) algorithm (a faithful, iterative, generic Go rewrite of
// Ruby's TSort), Kahn condensation DAG scheduling with deterministic priority
// tie-breaking, longest-path parallel wave partitioning, and cycle witnesses.
package tsort

import (
	"cmp"
	"container/heap"
	"fmt"
	"slices"
	"strings"
)

// Graph is the Go equivalent of Ruby TSort's tsort_each_node / tsort_each_child
// interface. An edge node -> child means "node depends on child" (child must
// come before node in topological order).
type Graph[T comparable] interface {
	EachNode(yield func(T) bool)
	EachChild(node T, yield func(T) bool)
}

// FuncGraph adapts standalone iteration functions into a Graph[T], mirroring
// Ruby's TSort.tsort(each_node, each_child) class methods.
type FuncGraph[T comparable] struct {
	Nodes    func(yield func(T) bool)
	Children func(node T, yield func(T) bool)
}

func (f FuncGraph[T]) EachNode(yield func(T) bool) {
	if f.Nodes != nil {
		f.Nodes(yield)
	}
}

func (f FuncGraph[T]) EachChild(node T, yield func(T) bool) {
	if f.Children != nil {
		f.Children(node, yield)
	}
}

// MapGraph adapts a Go map[T][]T (node -> dependencies) into a deterministic
// Graph[T] by iterating keys in sorted order.
type MapGraph[T cmp.Ordered] map[T][]T

func (m MapGraph[T]) EachNode(yield func(T) bool) {
	keys := make([]T, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !yield(k) {
			return
		}
	}
}

func (m MapGraph[T]) EachChild(node T, yield func(T) bool) {
	for _, c := range m[node] {
		if !yield(c) {
			return
		}
	}
}

// AdjGraph is an insertion-order-preserving adjacency list graph suitable for
// arbitrary comparable node types (not restricted to cmp.Ordered).
type AdjGraph[T comparable] struct {
	order    []T
	present  map[T]bool
	children map[T][]T
}

// NewAdjGraph creates an empty AdjGraph.
func NewAdjGraph[T comparable]() *AdjGraph[T] {
	return &AdjGraph[T]{
		present:  make(map[T]bool),
		children: make(map[T][]T),
	}
}

// AddNode registers node in the graph if not already present.
func (g *AdjGraph[T]) AddNode(node T) {
	if !g.present[node] {
		g.present[node] = true
		g.order = append(g.order, node)
	}
}

// AddEdge records a directed dependency edge (node depends on child),
// ensuring both endpoints exist in the graph without duplicating edges.
func (g *AdjGraph[T]) AddEdge(node, child T) {
	g.AddNode(node)
	g.AddNode(child)
	if !slices.Contains(g.children[node], child) {
		g.children[node] = append(g.children[node], child)
	}
}

// Children returns the direct dependencies of node.
func (g *AdjGraph[T]) Children(node T) []T {
	return g.children[node]
}

// Nodes returns all registered nodes in insertion order.
func (g *AdjGraph[T]) Nodes() []T {
	out := make([]T, len(g.order))
	copy(out, g.order)
	return out
}

func (g *AdjGraph[T]) EachNode(yield func(T) bool) {
	for _, n := range g.order {
		if !yield(n) {
			return
		}
	}
}

func (g *AdjGraph[T]) EachChild(node T, yield func(T) bool) {
	for _, c := range g.children[node] {
		if !yield(c) {
			return
		}
	}
}

// CyclicError is returned by strict topological sort when a cycle or self-loop
// is encountered.
type CyclicError[T comparable] struct {
	Component []T // All members of the strongly connected component
	CyclePath []T // Shortest concrete cycle witness, e.g. [a, b, a]
}

func (e *CyclicError[T]) Error() string {
	path := e.CyclePath
	if len(path) == 0 {
		path = e.Component
	}
	parts := make([]string, len(path))
	for i, n := range path {
		parts[i] = fmt.Sprint(n)
	}
	return fmt.Sprintf("topological sort failed: dependency cycle detected (%s)", strings.Join(parts, " -> "))
}

// tarjanState holds shared iterative Tarjan SCC traversal state.
type tarjanState[T comparable] struct {
	index    map[T]int
	lowlink  map[T]int
	onStack  map[T]bool
	selfLoop map[T]bool
	stack    []T
	nextID   int
}

func newTarjanState[T comparable]() *tarjanState[T] {
	return &tarjanState[T]{
		index:    make(map[T]int),
		lowlink:  make(map[T]int),
		onStack:  make(map[T]bool),
		selfLoop: make(map[T]bool),
	}
}

type tarjanFrame[T comparable] struct {
	node      T
	stackPos  int
	children  []T
	nextChild int
}

// visitFrom runs iterative Tarjan's SCC starting at start, yielding each
// strongly connected component in reverse topological order (children before
// parents). Returning false from yield stops traversal early.
func (ts *tarjanState[T]) visitFrom(start T, eachChild func(T, func(T) bool), yield func([]T) bool) bool {
	if _, seen := ts.index[start]; seen {
		return true
	}

	collectChildren := func(n T) []T {
		var kids []T
		eachChild(n, func(c T) bool {
			if c == n {
				ts.selfLoop[n] = true
			}
			kids = append(kids, c)
			return true
		})
		return kids
	}

	pushNode := func(n T) tarjanFrame[T] {
		id := ts.nextID
		ts.nextID++
		ts.index[n] = id
		ts.lowlink[n] = id
		pos := len(ts.stack)
		ts.stack = append(ts.stack, n)
		ts.onStack[n] = true
		return tarjanFrame[T]{
			node:     n,
			stackPos: pos,
			children: collectChildren(n),
		}
	}

	callStack := []tarjanFrame[T]{pushNode(start)}

	for len(callStack) > 0 {
		top := &callStack[len(callStack)-1]
		if top.nextChild < len(top.children) {
			child := top.children[top.nextChild]
			top.nextChild++

			if _, seen := ts.index[child]; !seen {
				callStack = append(callStack, pushNode(child))
			} else if ts.onStack[child] {
				if idx := ts.index[child]; idx < ts.lowlink[top.node] {
					ts.lowlink[top.node] = idx
				}
			}
			continue
		}

		// Finished exploring all children of top.node.
		finished := *top
		callStack = callStack[:len(callStack)-1]

		if len(callStack) > 0 {
			parent := callStack[len(callStack)-1].node
			if ts.lowlink[finished.node] < ts.lowlink[parent] {
				ts.lowlink[parent] = ts.lowlink[finished.node]
			}
		}

		if ts.lowlink[finished.node] == ts.index[finished.node] {
			comp := make([]T, len(ts.stack)-finished.stackPos)
			copy(comp, ts.stack[finished.stackPos:])
			ts.stack = ts.stack[:finished.stackPos]
			for _, n := range comp {
				ts.onStack[n] = false
			}
			if !yield(comp) {
				return false
			}
		}
	}
	return true
}

// EachSCC iterates over all strongly connected components of g in reverse
// topological order (children/dependencies before parents/dependents),
// matching Ruby's TSort.each_strongly_connected_component.
func EachSCC[T comparable](g Graph[T], yield func([]T) bool) {
	ts := newTarjanState[T]()
	g.EachNode(func(node T) bool {
		if _, seen := ts.index[node]; !seen {
			return ts.visitFrom(node, g.EachChild, yield)
		}
		return true
	})
}

// EachSCCFrom iterates over strongly connected components in the subgraph
// reachable from root, matching Ruby's TSort.each_strongly_connected_component_from.
func EachSCCFrom[T comparable](root T, eachChild func(T, func(T) bool), yield func([]T) bool) {
	ts := newTarjanState[T]()
	ts.visitFrom(root, eachChild, yield)
}

// SCCs returns all strongly connected components of g in reverse topological
// order (children before parents).
func SCCs[T comparable](g Graph[T]) [][]T {
	var out [][]T
	EachSCC(g, func(comp []T) bool {
		out = append(out, comp)
		return true
	})
	return out
}

// Sort returns a topologically sorted slice of nodes from children to parents
// (dependencies first, dependents last), matching Ruby's TSort.tsort.
// Unlike Ruby's TSort, self-loops (a -> a) are also detected and rejected.
func Sort[T comparable](g Graph[T]) ([]T, error) {
	ts := newTarjanState[T]()
	var out []T
	var cycErr error

	g.EachNode(func(node T) bool {
		if _, seen := ts.index[node]; !seen {
			return ts.visitFrom(node, g.EachChild, func(comp []T) bool {
				if len(comp) > 1 || (len(comp) == 1 && ts.selfLoop[comp[0]]) {
					cycErr = &CyclicError[T]{
						Component: comp,
						CyclePath: FindCyclePath(comp, g.EachChild),
					}
					return false
				}
				out = append(out, comp[0])
				return true
			})
		}
		return true
	})
	if cycErr != nil {
		return nil, cycErr
	}
	return out, nil
}

// FindCyclePath finds a shortest concrete directed cycle witness inside comp
// (e.g. [a, b, c, a]) for human-readable diagnostics.
func FindCyclePath[T comparable](comp []T, eachChild func(T, func(T) bool)) []T {
	if len(comp) == 0 {
		return nil
	}
	if len(comp) == 1 {
		return []T{comp[0], comp[0]}
	}
	inComp := make(map[T]bool, len(comp))
	for _, n := range comp {
		inComp[n] = true
	}

	// BFS from start := comp[0] inside inComp until we hit an edge back to start.
	start := comp[0]
	parent := make(map[T]T, len(comp))
	visited := map[T]bool{start: true}
	queue := []T{start}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		var found bool
		eachChild(cur, func(next T) bool {
			if !inComp[next] {
				return true
			}
			if next == start {
				// Reconstruct start -> ... -> cur -> start.
				found = true
				return false
			}
			if !visited[next] {
				visited[next] = true
				parent[next] = cur
				queue = append(queue, next)
			}
			return true
		})
		if found {
			var rev []T
			rev = append(rev, start)
			for n := cur; n != start; n = parent[n] {
				rev = append(rev, n)
			}
			rev = append(rev, start)
			slices.Reverse(rev)
			return rev
		}
	}

	// Fallback (should not happen for a valid SCC).
	out := make([]T, len(comp)+1)
	copy(out, comp)
	out[len(comp)] = comp[0]
	return out
}

// CondensedDAG represents the acyclic condensation graph produced by collapsing
// each strongly connected component of g into a single super-node.
type CondensedDAG[T comparable] struct {
	Components [][]T     // SCCs indexed 0..k-1
	NodeToComp map[T]int // Maps each node to its SCC index
	Deps       [][]int   // Component i depends on Component j (acyclic)
	RevDeps    [][]int   // Component j is depended on by Component i (acyclic)
	Cycles     [][]T     // Every SCC with len > 1 or a self-loop
	CyclePaths [][]T     // Concrete cycle witness path for each entry in Cycles
}

// Condense collapses all strongly connected components of g into an acyclic
// CondensedDAG.
func Condense[T comparable](g Graph[T]) *CondensedDAG[T] {
	ts := newTarjanState[T]()
	var comps [][]T
	g.EachNode(func(node T) bool {
		if _, seen := ts.index[node]; !seen {
			ts.visitFrom(node, g.EachChild, func(comp []T) bool {
				comps = append(comps, comp)
				return true
			})
		}
		return true
	})

	k := len(comps)
	dag := &CondensedDAG[T]{
		Components: comps,
		NodeToComp: make(map[T]int, len(ts.index)),
		Deps:       make([][]int, k),
		RevDeps:    make([][]int, k),
	}

	for cid, comp := range comps {
		for _, n := range comp {
			dag.NodeToComp[n] = cid
		}
		if len(comp) > 1 || (len(comp) == 1 && ts.selfLoop[comp[0]]) {
			dag.Cycles = append(dag.Cycles, comp)
			dag.CyclePaths = append(dag.CyclePaths, FindCyclePath(comp, g.EachChild))
		}
	}

	for cid, comp := range comps {
		depSeen := make(map[int]bool)
		for _, n := range comp {
			g.EachChild(n, func(child T) bool {
				depCID, ok := dag.NodeToComp[child]
				if !ok || depCID == cid || depSeen[depCID] {
					return true
				}
				depSeen[depCID] = true
				dag.Deps[cid] = append(dag.Deps[cid], depCID)
				dag.RevDeps[depCID] = append(dag.RevDeps[depCID], cid)
				return true
			})
		}
	}

	return dag
}

type compHeap struct {
	items []int
	less  func(a, b int) bool
}

func (h compHeap) Len() int           { return len(h.items) }
func (h compHeap) Less(i, j int) bool { return h.less(h.items[i], h.items[j]) }
func (h compHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *compHeap) Push(x any)        { h.items = append(h.items, x.(int)) }
func (h *compHeap) Pop() any {
	n := len(h.items)
	v := h.items[n-1]
	h.items = h.items[:n-1]
	return v
}

// SortCondensed computes a deterministic topological ordering (dependencies
// first, dependents last) by running Kahn's algorithm with a priority min-heap
// over the Tarjan condensation DAG.
//
//   - If less is non-nil, ties among ready components (and nodes inside a
//     cyclic SCC) are broken deterministically with less(a, b).
//   - If strict is true and any cycle/self-loop exists, returns *CyclicError[T].
//   - If strict is false, cyclic SCCs are grouped deterministically so all
//     external dependencies still precede the cycle and all dependents follow.
func SortCondensed[T comparable](g Graph[T], less func(a, b T) bool, strict bool) (order []T, cycles [][]T, err error) {
	dag := Condense(g)
	if strict && len(dag.Cycles) > 0 {
		return nil, dag.Cycles, &CyclicError[T]{
			Component: dag.Cycles[0],
			CyclePath: dag.CyclePaths[0],
		}
	}

	k := len(dag.Components)
	if less != nil {
		for i := range dag.Components {
			if len(dag.Components[i]) > 1 {
				slices.SortFunc(dag.Components[i], func(a, b T) int {
					if less(a, b) {
						return -1
					}
					if less(b, a) {
						return 1
					}
					return 0
				})
			}
		}
	}

	inDeg := make([]int, k)
	for cid := 0; cid < k; cid++ {
		inDeg[cid] = len(dag.Deps[cid])
	}

	h := &compHeap{
		less: func(a, b int) bool {
			if less != nil {
				na, nb := dag.Components[a][0], dag.Components[b][0]
				if less(na, nb) {
					return true
				}
				if less(nb, na) {
					return false
				}
			}
			return a < b
		},
	}
	heap.Init(h)
	for cid := 0; cid < k; cid++ {
		if inDeg[cid] == 0 {
			heap.Push(h, cid)
		}
	}

	order = make([]T, 0, len(dag.NodeToComp))
	for h.Len() > 0 {
		cid := heap.Pop(h).(int)
		order = append(order, dag.Components[cid]...)
		for _, parent := range dag.RevDeps[cid] {
			inDeg[parent]--
			if inDeg[parent] == 0 {
				heap.Push(h, parent)
			}
		}
	}

	return order, dag.Cycles, nil
}

// Waves partitions the graph into exact longest-path parallel layers over the
// Tarjan condensation DAG:
//   - Wave 0 contains SCCs with zero dependencies.
//   - Wave w contains SCCs whose deepest dependency sits in Wave w-1.
//   - For every acyclic edge (u depends on v), wave(v) < wave(u).
//   - Within each wave, nodes are sorted using less when non-nil.
func Waves[T comparable](g Graph[T], less func(a, b T) bool) (waves [][]T, cycles [][]T) {
	dag := Condense(g)
	k := len(dag.Components)
	if k == 0 {
		return nil, nil
	}

	inDeg := make([]int, k)
	level := make([]int, k)
	queue := make([]int, 0, k)
	for cid := 0; cid < k; cid++ {
		inDeg[cid] = len(dag.Deps[cid])
		if inDeg[cid] == 0 {
			queue = append(queue, cid)
		}
	}

	maxWave := 0
	for len(queue) > 0 {
		cid := queue[0]
		queue = queue[1:]
		if level[cid] > maxWave {
			maxWave = level[cid]
		}
		for _, parent := range dag.RevDeps[cid] {
			if level[cid]+1 > level[parent] {
				level[parent] = level[cid] + 1
			}
			inDeg[parent]--
			if inDeg[parent] == 0 {
				queue = append(queue, parent)
			}
		}
	}

	waves = make([][]T, maxWave+1)
	for cid, comp := range dag.Components {
		w := level[cid]
		waves[w] = append(waves[w], comp...)
	}
	if less != nil {
		for w := range waves {
			slices.SortFunc(waves[w], func(a, b T) int {
				if less(a, b) {
					return -1
				}
				if less(b, a) {
					return 1
				}
				return 0
			})
		}
	}
	return waves, dag.Cycles
}

// Reverse returns a new AdjGraph with every directed edge u -> v reversed to
// v -> u, preserving all nodes (including isolated nodes and implicit children).
func Reverse[T comparable](g Graph[T]) *AdjGraph[T] {
	rev := NewAdjGraph[T]()
	g.EachNode(func(node T) bool {
		rev.AddNode(node)
		g.EachChild(node, func(child T) bool {
			rev.AddEdge(child, node)
			return true
		})
		return true
	})
	return rev
}
