// Package discovery maintains a disposable local DNS projection of Docker.
package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JH0project/fabric/control/internal/runtime"
)

var labelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidLabel(s string) bool { return labelPattern.MatchString(s) }

type Endpoint struct {
	Node, ID, Service  string
	Addresses          []string
	Port               uint16
	PortName, Protocol string
}

func (e Endpoint) validate() error {
	if !ValidLabel(e.Node) || !ValidLabel(e.Service) || e.ID == "" || len(e.ID) > 256 || len(e.Addresses) == 0 {
		return fmt.Errorf("invalid endpoint identity or addresses")
	}
	if e.Port != 0 && (!ValidLabel(e.PortName) || (e.Protocol != "tcp" && e.Protocol != "udp")) {
		return fmt.Errorf("invalid endpoint port")
	}
	for _, s := range e.Addresses {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() || a.IsLinkLocalUnicast() {
			return fmt.Errorf("invalid advertised IP address")
		}
	}
	return nil
}

func (e Endpoint) target(zone string) string {
	sum := sha256.Sum256([]byte(e.Node + "\x00" + e.ID))
	return hex.EncodeToString(sum[:16]) + ".endpoint." + zone
}

// FromContainers is opt-in. Explicit addresses or one explicitly chosen Docker
// network prevent accidentally advertising an arbitrary bridge/loopback address.
// Invalid labelled containers are omitted; they must not preserve old records.
func FromContainers(node string, containers []runtime.Container) ([]Endpoint, []string) {
	var endpoints []Endpoint
	var invalid []string
	for _, c := range containers {
		service := c.Labels["fabric.discovery.service"]
		if service == "" || c.State != "running" || (c.Health != "" && c.Health != "healthy") {
			continue
		}
		e := Endpoint{Node: node, ID: c.ID, Service: service}
		if raw := c.Labels["fabric.discovery.addresses"]; raw != "" {
			for _, s := range strings.Split(raw, ",") {
				e.Addresses = append(e.Addresses, strings.TrimSpace(s))
			}
		} else if network := c.Labels["fabric.discovery.network"]; network != "" {
			e.Addresses = slices.Clone(c.Networks[network])
		}
		if raw := c.Labels["fabric.discovery.port"]; raw != "" {
			port, err := strconv.ParseUint(raw, 10, 16)
			if err != nil || port == 0 {
				invalid = append(invalid, c.ID)
				continue
			}
			e.Port = uint16(port)
			e.PortName = c.Labels["fabric.discovery.port-name"]
			if e.PortName == "" {
				e.PortName = service
			}
			e.Protocol = c.Labels["fabric.discovery.protocol"]
			if e.Protocol == "" {
				e.Protocol = "tcp"
			}
		}
		if err := e.validate(); err != nil {
			invalid = append(invalid, c.ID)
			continue
		}
		for i, s := range e.Addresses {
			e.Addresses[i] = netip.MustParseAddr(s).Unmap().String()
		}
		slices.Sort(e.Addresses)
		e.Addresses = slices.Compact(e.Addresses)
		endpoints = append(endpoints, e)
	}
	return endpoints, invalid
}
