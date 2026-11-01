package database

import (
	"context"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

const NodesMigration = "202610250001_nodes"

// Migrate is an explicit operator action. Historical migrations use fixed DDL,
// never the evolving Node model. Run one migration process at a time.
func (s *Store) Migrate(ctx context.Context) error {
	return retry(ctx, func() error {
		m := gormigrate.New(s.DB.WithContext(ctx), &gormigrate.Options{
			TableName: "schema_migrations", IDColumnName: "id", IDColumnSize: 128,
			UseTransaction: true, ValidateUnknownMigrations: true,
		}, []*gormigrate.Migration{{ID: NodesMigration, Migrate: func(tx *gorm.DB) error {
			return tx.Exec(`CREATE TABLE nodes (
				uuid UUID PRIMARY KEY,
				id INT8 NOT NULL UNIQUE CHECK (id > 0),
				region STRING NOT NULL CHECK (length(trim(region)) > 0),
				zone STRING NOT NULL CHECK (length(trim(zone)) > 0)
			)`).Error
		}}})
		return m.Migrate()
	})
}
