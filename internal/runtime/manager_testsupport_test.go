package runtime

// newTestManager builds a zero-value Manager and stamps its generation through
// the production reload path.
func newTestManager(generation uint64) *Manager {
	m := &Manager{}
	m.ReplaceGeneration(generation)
	return m
}
