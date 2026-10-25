package discovery

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
)

type snapshot struct{ names map[string][]Endpoint }

// Cache publishes complete immutable snapshots. Queries never call Docker or
// wait for observation. A nil snapshot means bootstrap has not succeeded yet.
type Cache struct {
	Zone, Node string
	current    atomic.Pointer[snapshot]
}

func NewCache(zone, node string) (*Cache, error) {
	zone = strings.TrimSuffix(strings.ToLower(zone), ".")
	// Leave space for maximum-length node, service and SRV port labels.
	if len(zone) > 40 || !ValidLabel(node) || node == "local" {
		return nil, fmt.Errorf("invalid discovery zone or node (local is reserved)")
	}
	for _, label := range strings.Split(zone, ".") {
		if !ValidLabel(label) {
			return nil, fmt.Errorf("invalid discovery zone")
		}
	}
	return &Cache{Zone: zone + ".", Node: node}, nil
}

func (c *Cache) Replace(endpoints []Endpoint) error {
	next := &snapshot{names: make(map[string][]Endpoint)}
	for _, e := range endpoints {
		if err := e.validate(); err != nil {
			return fmt.Errorf("invalid catalog snapshot")
		}
		e.Addresses = slices.Clone(e.Addresses)
		global := e.Service + ".service." + c.Zone
		node := e.Service + ".service." + e.Node + ".node." + c.Zone
		names := []string{global, node, e.target(c.Zone)}
		if e.Node == c.Node {
			names = append(names, e.Service+".service.local.node."+c.Zone)
		}
		for _, name := range names {
			next.names[name] = append(next.names[name], e)
			if e.Port != 0 && name != e.target(c.Zone) {
				srv := "_" + e.PortName + "._" + e.Protocol + "." + name
				next.names[srv] = append(next.names[srv], e)
			}
		}
	}
	// Preserve empty non-terminals so NXDOMAIN never denies existing children.
	for name := range next.names {
		for parent := name; parent != c.Zone; {
			_, parent, _ = strings.Cut(parent, ".")
			if _, exists := next.names[parent]; !exists {
				next.names[parent] = nil
			}
		}
	}
	c.current.Store(next)
	return nil
}

func (c *Cache) lookup(name string) (endpoints []Endpoint, ready, exists bool) {
	s := c.current.Load()
	if s == nil {
		return nil, false, false
	}
	endpoints, exists = s.names[name]
	return endpoints, true, exists
}
