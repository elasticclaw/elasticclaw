package claws

import "sync"

// Registry is the claw-connection registry: claw_id -> *Conn, guarded by its
// own RWMutex. It replaces the hub's former global Server mutex for the claw
// subsystem (phase-2 item 2.3): registry lookups no longer contend with
// config reads or user-session fanout.
//
// The zero value is ready to use (hand-built test servers construct
// &hub.Server{} without wiring), so every write path lazily initializes the
// map.
//
// Lock-ordering rule: the registry lock is a leaf lock — never acquire
// another subsystem lock (config, user registry) or a per-connection Conn.Mu
// while holding it, except inside Do/DoRead callbacks which may take
// Conn.Mu (registry lock -> Conn.Mu is the documented order, matching the
// pre-split behavior of the global mutex).
type Registry struct {
	mu    sync.RWMutex
	conns map[string]*Conn
}

// Get returns the connection for clawID, if registered.
func (r *Registry) Get(clawID string) (*Conn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cc, ok := r.conns[clawID]
	return cc, ok
}

// Lookup returns the connection for clawID or nil.
func (r *Registry) Lookup(clawID string) *Conn {
	cc, _ := r.Get(clawID)
	return cc
}

// Set registers (or replaces) the connection for clawID.
func (r *Registry) Set(clawID string, cc *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = make(map[string]*Conn)
	}
	r.conns[clawID] = cc
}

// Delete removes the connection for clawID.
func (r *Registry) Delete(clawID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, clawID)
}

// Len returns the number of registered connections.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.conns)
}

// IDs returns a snapshot of the registered claw IDs.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.conns))
	for id := range r.conns {
		ids = append(ids, id)
	}
	return ids
}

// Do runs fn with the registry write-locked, for compound
// read-modify-write operations (e.g. re-registration that migrates queued
// messages from the old connection) that must be atomic with respect to
// other registry writers. fn must not block on I/O or acquire other
// subsystem locks.
func (r *Registry) Do(fn func(conns map[string]*Conn)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = make(map[string]*Conn)
	}
	fn(r.conns)
}

// DoRead runs fn with the registry read-locked, for snapshot-style
// iteration. fn must not mutate the map, block on I/O, or acquire other
// subsystem locks.
func (r *Registry) DoRead(fn func(conns map[string]*Conn)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn(r.conns)
}
