package database

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Node contains only stable identity and placement. ID is assigned by Ansible,
// not by a database sequence. UUID is reused from the existing node registry.
type Node struct {
	UUID   uuid.UUID `json:"uuid" gorm:"type:uuid;primaryKey"`
	ID     int64     `json:"id" gorm:"not null;uniqueIndex;autoIncrement:false"`
	Region string    `json:"region" gorm:"not null"`
	Zone   string    `json:"zone" gorm:"not null"`
}

func ValidateNodes(nodes []Node) error {
	ids := map[int64]bool{}
	uuids := map[uuid.UUID]bool{}
	if len(nodes) == 0 {
		return errors.New("node import is empty")
	}
	for _, n := range nodes {
		if n.UUID == uuid.Nil || n.ID <= 0 || strings.TrimSpace(n.Region) == "" || strings.TrimSpace(n.Zone) == "" {
			return errors.New("node requires UUID, positive Ansible ID, region and zone")
		}
		if ids[n.ID] || uuids[n.UUID] {
			return errors.New("duplicate node UUID or Ansible ID")
		}
		ids[n.ID] = true
		uuids[n.UUID] = true
	}
	return nil
}

// ImportNodes is additive and atomic. Existing identity mappings cannot change;
// placement can be refreshed. Nodes missing from this input are retained.
func (s *Store) ImportNodes(ctx context.Context, nodes []Node) error {
	if err := ValidateNodes(nodes); err != nil {
		return err
	}
	return retry(ctx, func() error {
		return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, n := range nodes {
				var existing Node
				err := tx.Where("uuid = ? OR id = ?", n.UUID, n.ID).Take(&existing).Error
				if errors.Is(err, gorm.ErrRecordNotFound) {
					if err := tx.Create(&n).Error; err != nil {
						return err
					}
					continue
				}
				if err != nil {
					return err
				}
				if existing.UUID != n.UUID || existing.ID != n.ID {
					return errors.New("node UUID and Ansible ID conflict with existing mapping")
				}
				if err := tx.Model(&Node{}).Where("uuid = ?", n.UUID).Updates(map[string]any{"region": n.Region, "zone": n.Zone}).Error; err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	var nodes []Node
	err := s.DB.WithContext(ctx).Order("id").Find(&nodes).Error
	return nodes, err
}
