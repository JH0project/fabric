package runtime

import (
	"sort"
	"sync"
)

// View is a disposable materialized view of Docker, never desired state.
// Readers receive copies and cannot mutate stored labels.
type View struct {
	mu         sync.RWMutex
	containers map[string]Container
}

func (v *View) Replace(containers []Container) {
	next := make(map[string]Container, len(containers))
	for _, c := range containers {
		next[c.ID] = clone(c)
	}
	v.mu.Lock()
	v.containers = next
	v.mu.Unlock()
}

func (v *View) Upsert(c Container) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.containers == nil {
		v.containers = make(map[string]Container)
	}
	v.containers[c.ID] = clone(c)
}

func (v *View) Remove(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.containers, id)
}

func (v *View) Snapshot() []Container {
	v.mu.RLock()
	defer v.mu.RUnlock()
	result := make([]Container, 0, len(v.containers))
	for _, c := range v.containers {
		result = append(result, clone(c))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func clone(c Container) Container {
	if c.Labels != nil {
		labels := make(map[string]string, len(c.Labels))
		for k, value := range c.Labels {
			labels[k] = value
		}
		c.Labels = labels
	}
	return c
}
