package guard

// matcher.go: the phase-1 literal prefilter. All prefilter literals of a
// Scanner (rule literals, encoded-channel probe variants, known-secret
// variants) are compiled into one Aho-Corasick automaton, so a clean body
// costs a single pass over the input instead of one bytes.Contains per
// literal — the automaton is a pure internal detail of the package (same
// prefilter semantics as bytes.Contains on every literal: identical hit
// sets). Construction happens once per Scanner build (reload), never on the
// request path.

// acEdge is one trie transition.
type acEdge struct {
	c  byte
	to int
}

// acNode is one Aho-Corasick state. edges is sorted by byte; most states have
// one or two, so a linear scan beats a map lookup.
type acNode struct {
	edges []acEdge
	fail  int
	// out lists needle ids ending exactly at this state.
	out []int
	// dict is the nearest fail ancestor with a non-empty out, or -1 — the
	// standard dictionary-suffix link that keeps match emission proportional
	// to the number of matches.
	dict int
}

// edge returns the transition for c, if any.
func (n *acNode) edge(c byte) (int, bool) {
	for _, e := range n.edges {
		if e.c == c {
			return e.to, true
		}
	}
	return 0, false
}

// acMatcher is a compiled Aho-Corasick automaton over byte needles. root is
// the dense first-level transition table (the hot path: most bytes of a body
// are scanned while the automaton sits at the root).
type acMatcher struct {
	nodes []acNode
	root  [256]int // 0 = no transition (stay at root)
}

// acBuilder accumulates needles before compile.
type acBuilder struct {
	next []map[byte]int
	out  [][]int
}

func newACBuilder() *acBuilder {
	return &acBuilder{next: []map[byte]int{{}}, out: make([][]int, 1)}
}

// add inserts needle with the given id. Duplicate needles are allowed: every
// id is recorded on the shared terminal state.
func (b *acBuilder) add(needle []byte, id int) {
	v := 0
	for _, c := range needle {
		n, ok := b.next[v][c]
		if !ok {
			n = len(b.next)
			b.next = append(b.next, map[byte]int{})
			b.out = append(b.out, nil)
			b.next[v][c] = n
		}
		v = n
	}
	b.out[v] = append(b.out[v], id)
}

// compile computes fail and dictionary-suffix links (BFS from the root) and
// flattens the transition maps into sorted edge slices plus the root table.
func (b *acBuilder) compile() *acMatcher {
	n := len(b.next)
	fail := make([]int, n)
	var queue []int
	for _, u := range b.next[0] {
		queue = append(queue, u)
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for c, u := range b.next[v] {
			// fail[u] is the longest proper suffix of u's path that is also
			// a trie state: walk v's fail chain to the first state with a
			// c-transition (root included).
			f := fail[v]
			found := 0
			for {
				if g, ok := b.next[f][c]; ok && g != u {
					found = g
					break
				}
				if f == 0 {
					break
				}
				f = fail[f]
			}
			fail[u] = found
			queue = append(queue, u)
		}
	}
	m := &acMatcher{nodes: make([]acNode, n)}
	for c, u := range b.next[0] {
		m.root[c] = u
	}
	for v := 0; v < n; v++ {
		nd := &m.nodes[v]
		nd.fail = fail[v]
		nd.out = b.out[v]
		for c, u := range b.next[v] {
			nd.edges = append(nd.edges, acEdge{c: c, to: u})
		}
		for i := 1; i < len(nd.edges); i++ {
			for j := i; j > 0 && nd.edges[j-1].c > nd.edges[j].c; j-- {
				nd.edges[j-1], nd.edges[j] = nd.edges[j], nd.edges[j-1]
			}
		}
		nd.dict = -1
	}
	// Dictionary-suffix links: nearest fail-chain ancestor carrying output.
	// Computed in BFS order so fail ancestors (always shallower, enqueued
	// earlier) are finalized first.
	queue = queue[:0]
	for _, u := range b.next[0] {
		queue = append(queue, u)
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		f := fail[v]
		if len(b.out[f]) > 0 {
			m.nodes[v].dict = f
		} else {
			m.nodes[v].dict = m.nodes[f].dict
		}
		for _, u := range b.next[v] {
			queue = append(queue, u)
		}
	}
	return m
}

// search walks body once, calling fn(id, end) for every needle occurrence,
// where end is the exclusive end offset (occurrence = body[end-len(needle):end]).
// Calls for one needle arrive in increasing end order.
func (m *acMatcher) search(body []byte, fn func(id, end int)) {
	v := 0
	for i, c := range body {
		advanced := false
		for v != 0 {
			if to, ok := m.nodes[v].edge(c); ok {
				v = to
				advanced = true
				break
			}
			v = m.nodes[v].fail
		}
		if !advanced {
			v = m.root[c]
			if v == 0 {
				continue
			}
		}
		if len(m.nodes[v].out) > 0 {
			for _, id := range m.nodes[v].out {
				fn(id, i+1)
			}
		}
		for d := m.nodes[v].dict; d >= 0; d = m.nodes[d].dict {
			for _, id := range m.nodes[d].out {
				fn(id, i+1)
			}
		}
	}
}
