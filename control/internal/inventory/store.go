package inventory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/JH0project/fabric/control/internal/database"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

type Store struct {
	DB        *gorm.DB
	Pool      *sql.DB // The same pool used by GORM; never a second connection pool.
	operation *operation
}

func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	op, err := newOperation("persist")
	if err != nil {
		return nil, err
	}
	registry, err := database.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	pool, err := registry.DB.DB()
	if err != nil {
		registry.Close()
		return nil, err
	}
	return &Store{DB: registry.DB, Pool: pool, operation: op}, nil
}
func (s *Store) Close() { s.Pool.Close() }
func (s *Store) Apply(ctx context.Context, snapshot Snapshot) (err error) {
	ctx, finish := s.operation.start(ctx)
	defer func() { finish(err) }()
	if err := snapshot.Validate(); err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		err := s.apply(ctx, snapshot)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "40001" {
			return err
		}
		timer := time.NewTimer(time.Duration(10*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("inventory transaction serialization retries exhausted")
}
func (s *Store) apply(ctx context.Context, v Snapshot) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.applyTransaction(tx, v)
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
}
func (s *Store) applyTransaction(tx *gorm.DB, v Snapshot) error {
	// The FK requires a registered node; observations cannot invent node identity.
	if err := tx.Exec(`INSERT INTO node_observations(node_id,observed_at) VALUES($1,$2) ON CONFLICT(node_id) DO NOTHING`, v.NodeID.String(), v.ObservedAt).Error; err != nil {
		return err
	}
	var epoch, revision, complete uint64
	if err := tx.Raw(`SELECT epoch,revision,complete_revision FROM node_observations WHERE node_id=$1 FOR UPDATE`, v.NodeID.String()).Row().Scan(&epoch, &revision, &complete); err != nil {
		return err
	}
	if v.Epoch < epoch {
		return nil
	}
	newer := v.Epoch > epoch || v.Revision > revision
	if !newer && (!v.Complete || v.Revision <= complete) {
		return nil
	}
	if v.Epoch > epoch {
		complete = 0
	}
	ids := make([]string, 0, len(v.Instances))
	rows := make([]string, 0, len(v.Instances))
	args := make([]any, 0, len(v.Instances)*14)
	for _, i := range v.Instances {
		ids = append(ids, i.RuntimeID)
		labels, _ := json.Marshal(i.Labels)
		networks, _ := json.Marshal(i.NetworkAttachments)
		p := len(args) + 1
		rows = append(rows, fmt.Sprintf("($%d,$%d,'docker',$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,'observed',$%d,$%d,$%d,$%d)", p, p+1, p+2, p+3, p+4, p+5, p+6, p+7, p+8, p+9, p+10, p+11, p+12, p+13))
		args = append(args, i.ID.String(), i.NodeID.String(), i.RuntimeID, i.Name, i.Image, i.ImageID, labels, networks, i.RuntimeState, i.HealthState, i.CreatedAt, i.ObservedAt, v.Epoch, v.Revision)
	}
	// One statement per snapshot avoids a network/consensus round trip for each
	// container. Validation bounds the batch below the PostgreSQL parameter limit.
	if len(rows) > 0 {
		err := tx.Exec(`INSERT INTO runtime_instances
            (id,node_id,runtime_type,runtime_id,name,image,image_id,labels,network_attachments,runtime_state,health_state,origin,created_at,observed_at,epoch,revision)
            VALUES `+strings.Join(rows, ",")+`
            ON CONFLICT(node_id,runtime_type,runtime_id) DO UPDATE SET
            name=excluded.name,image=excluded.image,image_id=excluded.image_id,labels=excluded.labels,network_attachments=excluded.network_attachments,
            runtime_state=excluded.runtime_state,health_state=excluded.health_state,observed_at=excluded.observed_at,updated_at=now(),removed_at=NULL,epoch=excluded.epoch,revision=excluded.revision
            WHERE runtime_instances.epoch<excluded.epoch OR (runtime_instances.epoch=excluded.epoch AND runtime_instances.revision<excluded.revision)`, args...).Error
		if err != nil {
			return err
		}
	}
	// Never remove future preallocated rows with no runtime binding. A delayed
	// complete snapshot can retire only observations at or before its version.
	removeSQL := `UPDATE runtime_instances SET runtime_state='removed',removed_at=COALESCE(removed_at,$2),observed_at=$2,updated_at=now(),epoch=$3,revision=$4
         WHERE node_id=$1 AND runtime_id IS NOT NULL AND runtime_state!='removed' AND (epoch<$3 OR (epoch=$3 AND revision<=$4)) AND `
	if v.Complete && v.Revision > complete {
		if err := tx.Exec(removeSQL+`NOT (runtime_id=ANY($5::STRING[]))`, v.NodeID.String(), v.ObservedAt, v.Epoch, v.Revision, ids).Error; err != nil {
			return err
		}
		complete = v.Revision
	}
	if len(v.Removed) > 0 {
		rows = make([]string, 0, len(v.Removed))
		args = make([]any, 0, len(v.Removed)*6)
		for _, id := range v.Removed {
			// A delete may precede the first received upsert. Persist its version
			// even for an unknown runtime ID so a delayed full snapshot cannot
			// insert it again. Existing identity/origin/associations stay intact.
			p := len(args) + 1
			rows = append(rows, fmt.Sprintf("($%d,$%d,'docker',$%d,'removed','observed',$%d,$%d,$%d,$%d)", p, p+1, p+2, p+3, p+3, p+4, p+5))
			args = append(args, ObservedID(v.NodeID, "docker", id).String(), v.NodeID.String(), id, v.ObservedAt, v.Epoch, v.Revision)
		}
		err := tx.Exec(`INSERT INTO runtime_instances
                (id,node_id,runtime_type,runtime_id,runtime_state,origin,observed_at,removed_at,epoch,revision)
                VALUES `+strings.Join(rows, ",")+`
                ON CONFLICT(node_id,runtime_type,runtime_id) DO UPDATE SET
                runtime_state='removed',removed_at=COALESCE(runtime_instances.removed_at,excluded.removed_at),
                observed_at=excluded.observed_at,updated_at=now(),epoch=excluded.epoch,revision=excluded.revision
                WHERE runtime_instances.epoch<excluded.epoch OR (runtime_instances.epoch=excluded.epoch AND runtime_instances.revision<excluded.revision)`, args...).Error
		if err != nil {
			return err
		}
	}
	if newer {
		return tx.Exec(`UPDATE node_observations SET epoch=$2,revision=$3,complete_revision=$4,observed_at=$5,updated_at=now() WHERE node_id=$1`, v.NodeID.String(), v.Epoch, v.Revision, complete, v.ObservedAt).Error
	}
	return tx.Exec(`UPDATE node_observations SET complete_revision=$2 WHERE node_id=$1`, v.NodeID.String(), complete).Error
}
