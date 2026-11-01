package database

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RuntimeInstance records a physical runtime incarnation. Associations and
// fabric_managed origin are reserved, never assigned by observed snapshots.
type RuntimeInstance struct {
	ID                 uuid.UUID           `json:"id" gorm:"type:uuid;primaryKey"`
	NodeID             uuid.UUID           `json:"node_id" gorm:"type:uuid;not null"`
	ServiceID          *uuid.UUID          `json:"service_id,omitempty" gorm:"type:uuid"`
	DeploymentID       *uuid.UUID          `json:"deployment_id,omitempty" gorm:"type:uuid"`
	RolloutID          *uuid.UUID          `json:"rollout_id,omitempty" gorm:"type:uuid"`
	RuntimeType        string              `json:"runtime_type"`
	RuntimeID          *string             `json:"runtime_id"`
	Name               string              `json:"name"`
	Image              string              `json:"image"`
	ImageID            string              `json:"image_id"`
	Labels             map[string]string   `json:"labels" gorm:"serializer:json;type:jsonb"`
	NetworkAttachments map[string][]string `json:"network_attachments" gorm:"serializer:json;type:jsonb"`
	RuntimeState       string              `json:"runtime_state"`
	HealthState        string              `json:"health_state"`
	Origin             string              `json:"origin"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	ObservedAt         time.Time           `json:"observed_at"`
	RemovedAt          *time.Time          `json:"removed_at,omitempty"`
	Epoch              uint64              `json:"epoch" gorm:"type:bigint"`
	Revision           uint64              `json:"revision" gorm:"type:bigint"`
}

// NodeObservation keeps synchronization state out of the four-field Node model.
type NodeObservation struct {
	NodeID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	Epoch            uint64    `gorm:"type:bigint"`
	Revision         uint64    `gorm:"type:bigint"`
	CompleteRevision uint64    `gorm:"type:bigint"`
	ObservedAt       time.Time
	UpdatedAt        time.Time
}

type RuntimeObservation struct {
	RuntimeInstance  `gorm:"embedded"`
	ObservationStale bool `json:"observation_stale"`
}

func (RuntimeObservation) TableName() string { return "runtime_instance_observations" }

// RuntimeInstances includes removal history and computed freshness. Reads are
// bounded; neither a stale node nor an unobserved allocation is marked stopped.
func (s *Store) RuntimeInstances(ctx context.Context, node uuid.UUID) ([]RuntimeObservation, error) {
	var result []RuntimeObservation
	err := s.DB.WithContext(ctx).Where("node_id = ?", node).Order("created_at DESC, id").Limit(1000).Find(&result).Error
	return result, err
}
