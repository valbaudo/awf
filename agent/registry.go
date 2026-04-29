package agent

import (
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Registry is the CLI-side store of registered Adapters. The zero value is
// usable (no New() constructor — matches bytes.Buffer / sync.Mutex stdlib
// shape). Goroutine-safe: Register takes a write lock; Lookup and Refs
// take a read lock (Phase 3 parallel branches Lookup concurrently after
// Phase 5 slice 5.2 wires AgentStep).
//
// Registry satisfies the Resolver interface — the read-only subset the
// engine's dispatcher (slice 5.2) takes. Use *Registry at CLI start-time
// (where Register matters); the engine works with Resolver only.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// Register adds an adapter under its Ref(). Returns *ErrAdapterAlreadyRegistered
// if a different adapter is already registered under that ref. Empty refs
// are rejected with a non-typed error (configuration bug; not worth a
// dedicated type since callers shouldn't be passing empty refs).
func (r *Registry) Register(a Adapter) error {
	ref := a.Ref()
	if ref == "" {
		return fmt.Errorf("agent: cannot register adapter with empty Ref()")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.adapters == nil {
		r.adapters = make(map[string]Adapter)
	}
	if existing, ok := r.adapters[ref]; ok && existing != a {
		return &ErrAdapterAlreadyRegistered{Ref: ref}
	}
	r.adapters[ref] = a
	return nil
}

// Lookup returns the registered Adapter for ref (true) or (nil, false).
// Implements Resolver. Concurrency-safe.
func (r *Registry) Lookup(ref string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[ref]
	return a, ok
}

// Refs returns all registered refs in sorted order. Phase 6 obs reads this
// for the `awf inspect` output; sort guarantees deterministic display.
//
// Go 1.23+ idiom: maps.Keys returns an iter.Seq[string] (lazy iteration over
// the map's keys), slices.Sorted consumes it into a sorted slice in one
// allocation. Equivalent in semantics to the manual loop + sort.Strings
// version, but cleaner and one fewer named intermediate.
func (r *Registry) Refs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.adapters))
}
