// Package inventory defines observation-only RuntimeInstances and the portable
// snapshot protocol shared by agents and the persistence consumer.
package inventory

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/google/uuid"
)

const SchemaVersion = 1
const MaxMessageBytes = 1 << 20
const MaxInstances = 2048

type RuntimeInstance struct {
	ID                 uuid.UUID           `json:"id"`
	NodeID             uuid.UUID           `json:"node_id"`
	ServiceID          *uuid.UUID          `json:"service_id,omitempty"`
	DeploymentID       *uuid.UUID          `json:"deployment_id,omitempty"`
	RolloutID          *uuid.UUID          `json:"rollout_id,omitempty"`
	RuntimeType        string              `json:"runtime_type"`
	RuntimeID          string              `json:"runtime_id"`
	Name               string              `json:"name"`
	Image              string              `json:"image"`
	ImageID            string              `json:"image_id"`
	Labels             map[string]string   `json:"labels"`
	NetworkAttachments map[string][]string `json:"network_attachments"`
	RuntimeState       string              `json:"runtime_state"`
	HealthState        string              `json:"health_state"`
	Origin             string              `json:"origin"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	ObservedAt         time.Time           `json:"observed_at"`
}

// Snapshot carries current known instances. Only Complete snapshots may remove
// absent instances; Removed contains explicit Docker-not-found observations.
type Snapshot struct {
	SchemaVersion int               `json:"schema_version"`
	NodeID        uuid.UUID         `json:"node_id"`
	NodeName      string            `json:"node_name"`
	Epoch         uint64            `json:"epoch"`
	Revision      uint64            `json:"revision"`
	ObservedAt    time.Time         `json:"observed_at"`
	Complete      bool              `json:"complete"`
	Instances     []RuntimeInstance `json:"instances"`
	Removed       []string          `json:"removed,omitempty"`
}

func Newer(a, b Snapshot) bool {
	return a.Epoch > b.Epoch || (a.Epoch == b.Epoch && a.Revision > b.Revision)
}

// ObservedID is a stable Fabric UUID, independent of process lifetime and name.
// The versioned namespace contract must remain stable once records exist.
func ObservedID(node uuid.UUID, runtimeType, runtimeID string) uuid.UUID {
	return uuid.NewSHA1(node, []byte("fabric/runtime-instance/v1/"+runtimeType+"/"+runtimeID))
}

func Project(node uuid.UUID, name string, epoch, revision uint64, observation runtime.Observation, now time.Time) Snapshot {
	s := Snapshot{SchemaVersion: SchemaVersion, NodeID: node, NodeName: name, Epoch: epoch, Revision: revision, ObservedAt: now.UTC(), Complete: observation.Complete, Removed: append([]string(nil), observation.Removed...)}
	for _, c := range observation.Containers {
		observed := c.ObservedAt
		if observed.IsZero() {
			observed = now
		}
		created := c.CreatedAt
		if created.IsZero() {
			created = now
		}
		labels := make(map[string]string)
		for k, v := range c.Labels {
			if AllowedLabel(k) && len(v) <= 2048 {
				labels[k] = v
			}
		}
		networks := make(map[string][]string)
		for network, addresses := range c.Networks {
			networks[network] = append([]string(nil), addresses...)
		}
		s.Instances = append(s.Instances, RuntimeInstance{ID: ObservedID(node, "docker", c.ID), NodeID: node, RuntimeType: "docker", RuntimeID: c.ID, Name: c.Name, Image: c.Image, ImageID: c.ImageID, Labels: labels, NetworkAttachments: networks, RuntimeState: c.State, HealthState: c.Health, Origin: "observed", CreatedAt: created.UTC(), UpdatedAt: now.UTC(), ObservedAt: observed.UTC()})
	}
	return s
}

func AllowedLabel(k string) bool {
	switch k {
	case "fabric.discovery.service", "fabric.discovery.network", "fabric.discovery.addresses", "fabric.discovery.port", "fabric.discovery.port-name", "fabric.discovery.protocol", "fabric.discovery.scope", "fabric.discovery.global-addresses", "com.docker.compose.project", "com.docker.compose.service", "coolify.applicationId":
		return true
	}
	return false
}

func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 63 || s == "local" {
		return false
	}
	for i, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0 && i < len(s)-1 {
			continue
		}
		return false
	}
	return true
}

func (s Snapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion || s.NodeID == uuid.Nil || !ValidName(s.NodeName) || s.Epoch == 0 || s.Revision == 0 || s.Epoch > 1<<63-1 || s.Revision > 1<<63-1 || s.ObservedAt.IsZero() || len(s.Instances) > MaxInstances || len(s.Removed) > MaxInstances {
		return errors.New("invalid inventory snapshot envelope")
	}
	seen := make(map[string]bool)
	for _, i := range s.Instances {
		if i.NodeID != s.NodeID || i.RuntimeType != "docker" || i.RuntimeID == "" || len(i.RuntimeID) > 256 || seen[i.RuntimeID] || i.ID != ObservedID(i.NodeID, i.RuntimeType, i.RuntimeID) || i.Origin != "observed" || i.ServiceID != nil || i.DeploymentID != nil || i.RolloutID != nil {
			return errors.New("invalid observed instance identity or authority")
		}
		seen[i.RuntimeID] = true
		if len(i.Name) > 256 || len(i.Image) > 2048 || len(i.ImageID) > 256 || len(i.RuntimeState) > 32 || len(i.HealthState) > 32 || i.ObservedAt.IsZero() || i.CreatedAt.IsZero() || i.UpdatedAt.IsZero() || i.ObservedAt.After(s.ObservedAt) {
			return errors.New("invalid instance metadata")
		}
		if len(i.Labels) > 16 || len(i.NetworkAttachments) > 64 {
			return errors.New("inventory metadata limit exceeded")
		}
		for k, v := range i.Labels {
			if !AllowedLabel(k) || len(v) > 2048 {
				return errors.New("unapproved instance label")
			}
		}
		for n, addresses := range i.NetworkAttachments {
			if len(n) > 256 || len(addresses) > 8 {
				return errors.New("network metadata limit exceeded")
			}
			for _, a := range addresses {
				addr, err := netip.ParseAddr(a)
				if err != nil || addr.Zone() != "" {
					return fmt.Errorf("invalid observed network address")
				}
			}
		}
	}
	for _, id := range s.Removed {
		if id == "" || len(id) > 256 || seen[id] {
			return errors.New("invalid removal set")
		}
		seen[id] = true
	}
	return nil
}

func (i RuntimeInstance) Container() runtime.Container {
	return runtime.Container{ID: i.RuntimeID, Name: i.Name, Image: i.Image, ImageID: i.ImageID, CreatedAt: i.CreatedAt, State: i.RuntimeState, Health: i.HealthState, Labels: i.Labels, Networks: i.NetworkAttachments}
}

func NativeIPv6(raw string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(raw))
	return err == nil && a.Is6() && !a.Is4In6() && a.Zone() == ""
}
