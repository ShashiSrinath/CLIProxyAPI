package auth

import (
	"maps"
	"sync"
)

// priorityOverrides holds dynamic scheduler priorities keyed by auth ID. They
// take precedence over the configured "priority" attribute and are used by
// plan-aware smart routing.
var priorityOverrides = struct {
	sync.RWMutex
	values map[string]int
}{values: map[string]int{}}

func priorityOverride(authID string) (int, bool) {
	priorityOverrides.RLock()
	defer priorityOverrides.RUnlock()
	value, ok := priorityOverrides.values[authID]
	return value, ok
}

// PriorityOverrides returns a copy of the active dynamic priority overrides.
func PriorityOverrides() map[string]int {
	priorityOverrides.RLock()
	defer priorityOverrides.RUnlock()
	return maps.Clone(priorityOverrides.values)
}

// SetPriorityOverrides replaces the dynamic priority overrides and refreshes the
// scheduler entries of every auth whose effective priority changed. A nil or
// empty map restores configured priorities.
func (m *Manager) SetPriorityOverrides(overrides map[string]int) {
	priorityOverrides.Lock()
	previous := priorityOverrides.values
	next := make(map[string]int, len(overrides))
	for id, value := range overrides {
		next[id] = value
	}
	priorityOverrides.values = next
	priorityOverrides.Unlock()

	if m == nil || m.scheduler == nil {
		return
	}
	changed := map[string]struct{}{}
	for id, value := range next {
		if old, ok := previous[id]; !ok || old != value {
			changed[id] = struct{}{}
		}
	}
	for id := range previous {
		if _, ok := next[id]; !ok {
			changed[id] = struct{}{}
		}
	}
	for id := range changed {
		if auth, ok := m.GetByID(id); ok && auth != nil {
			m.scheduler.upsertAuth(auth)
		}
	}
}
