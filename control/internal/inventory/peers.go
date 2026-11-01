package inventory

import (
	"sync"
	"time"

	"github.com/JH0project/fabric/control/internal/discovery"
	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/google/uuid"
)

type versioned struct {
	instance RuntimeInstance
	revision uint64
	removed  bool
}
type nodeView struct {
	head     Snapshot
	complete uint64
	entries  map[string]versioned
}

// Peers is a disposable projection. Expiry never asserts a container stopped.
type Peers struct {
	mu         sync.Mutex
	local      uuid.UUID
	nodes      map[uuid.UUID]*nodeView
	StaleAfter time.Duration
}

func NewPeers(local uuid.UUID, stale time.Duration) *Peers {
	return &Peers{local: local, nodes: map[uuid.UUID]*nodeView{}, StaleAfter: stale}
}
func (p *Peers) Apply(s Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.nodes[s.NodeID]
	if n == nil {
		if len(p.nodes) >= 256 && s.NodeID != p.local {
			return
		}
		n = &nodeView{entries: map[string]versioned{}}
		p.nodes[s.NodeID] = n
	}
	if s.Epoch < n.head.Epoch {
		return
	}
	if s.Epoch > n.head.Epoch {
		n.entries = map[string]versioned{}
		n.complete = 0
		n.head = Snapshot{}
	}
	newer := Newer(s, n.head)
	if !s.Complete {
		additional := 0
		for _, i := range s.Instances {
			if _, ok := n.entries[i.RuntimeID]; !ok {
				additional++
			}
		}
		for _, id := range s.Removed {
			if _, ok := n.entries[id]; !ok {
				additional++
			}
		}
		if len(n.entries)+additional > 2*MaxInstances {
			return
		}
	}
	if !newer && (!s.Complete || s.Revision <= n.complete) {
		return
	}
	present := map[string]bool{}
	for _, i := range s.Instances {
		present[i.RuntimeID] = true
		if old, ok := n.entries[i.RuntimeID]; !ok || s.Revision > old.revision {
			n.entries[i.RuntimeID] = versioned{instance: i, revision: s.Revision}
		}
	}
	for _, id := range s.Removed {
		if old, ok := n.entries[id]; !ok || old.revision <= s.Revision {
			n.entries[id] = versioned{revision: s.Revision, removed: true}
		}
	}
	if s.Complete && s.Revision > n.complete {
		for id, i := range n.entries {
			if !present[id] && i.revision <= s.Revision {
				delete(n.entries, id)
			}
		}
		n.complete = s.Revision
	}
	if newer {
		n.head = s
		n.head.Instances = nil
		n.head.Removed = nil
	}
}

func (p *Peers) Endpoints(now time.Time) []discovery.Endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result []discovery.Endpoint
	for id, n := range p.nodes {
		deadline := n.head.ObservedAt.Add(p.StaleAfter)
		if id != p.local && !deadline.After(now) {
			continue
		}
		for _, v := range n.entries {
			if v.removed {
				continue
			}
			c := v.instance.Container()
			if id != p.local {
				if c.Labels["fabric.discovery.scope"] != "global" {
					continue
				}
				if addresses := c.Labels["fabric.discovery.global-addresses"]; addresses != "" {
					c.Labels = cloneLabels(c.Labels)
					c.Labels["fabric.discovery.addresses"] = addresses
				}
			}
			endpoints, _ := discovery.FromContainers(n.head.NodeName, []runtime.Container{c})
			for _, e := range endpoints {
				var ipv6 []string
				for _, a := range e.Addresses {
					if NativeIPv6(a) {
						ipv6 = append(ipv6, a)
					}
				}
				if len(ipv6) == 0 {
					continue
				}
				e.Addresses = ipv6
				if id != p.local {
					e.ValidUntil = deadline
				}
				result = append(result, e)
			}
		}
	}
	return result
}
func cloneLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for k, v := range labels {
		result[k] = v
	}
	return result
}
