package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool      *pgxpool.Pool
	operation *operation
}

func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Query().Get("sslmode") != "verify-full" || u.Query().Get("sslrootcert") == "" {
		return nil, errors.New("inventory database URL requires verify-full and a CA path")
	}
	certificate, key := u.Query().Get("sslcert"), u.Query().Get("sslkey")
	if (certificate == "") != (key == "") {
		return nil, errors.New("database client certificate and key must be supplied together")
	}
	password := ""
	if u.User != nil {
		password, _ = u.User.Password()
	}
	if certificate == "" && password == "" {
		return nil, errors.New("database requires password or client certificate authentication")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !NativeIPv6(u.Hostname()) {
		return nil, errors.New("inventory database endpoint must use IPv6")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid inventory database configuration")
	}
	if config.ConnConfig.TLSConfig == nil || config.ConnConfig.TLSConfig.InsecureSkipVerify {
		return nil, errors.New("inventory database TLS verification is required")
	}
	config.ConnConfig.DialFunc = DialIPv6
	config.ConnConfig.ConnectTimeout = 3 * time.Second
	config.ConnConfig.Fallbacks = nil
	config.MaxConns = 4
	op, err := newOperation("persist")
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("create inventory database pool failed")
	}
	return &Store{Pool: pool, operation: op}, nil
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
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO fabric_nodes(id,name,observed_at) VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING`, v.NodeID.String(), v.NodeName, v.ObservedAt); err != nil {
		return err
	}
	var epoch, revision, complete uint64
	if err = tx.QueryRow(ctx, `SELECT epoch,revision,complete_revision FROM fabric_nodes WHERE id=$1 FOR UPDATE`, v.NodeID.String()).Scan(&epoch, &revision, &complete); err != nil {
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
	for _, i := range v.Instances {
		ids = append(ids, i.RuntimeID)
		labels, _ := json.Marshal(i.Labels)
		networks, _ := json.Marshal(i.NetworkAttachments)
		_, err = tx.Exec(ctx, `INSERT INTO runtime_instances
            (id,node_id,runtime_type,runtime_id,name,image,image_id,labels,network_attachments,runtime_state,health_state,origin,created_at,observed_at,epoch,revision)
            VALUES($1,$2,'docker',$3,$4,$5,$6,$7,$8,$9,$10,'observed',$11,$12,$13,$14)
            ON CONFLICT(node_id,runtime_type,runtime_id) DO UPDATE SET
            name=excluded.name,image=excluded.image,image_id=excluded.image_id,labels=excluded.labels,network_attachments=excluded.network_attachments,
            runtime_state=excluded.runtime_state,health_state=excluded.health_state,observed_at=excluded.observed_at,updated_at=now(),removed_at=NULL,epoch=excluded.epoch,revision=excluded.revision
            WHERE runtime_instances.epoch<excluded.epoch OR (runtime_instances.epoch=excluded.epoch AND runtime_instances.revision<excluded.revision)`, i.ID.String(), i.NodeID.String(), i.RuntimeID, i.Name, i.Image, i.ImageID, labels, networks, i.RuntimeState, i.HealthState, i.CreatedAt, i.ObservedAt, v.Epoch, v.Revision)
		if err != nil {
			return err
		}
	}
	// Never remove future preallocated rows with no runtime binding. A delayed
	// complete snapshot can retire only observations at or before its version.
	removeSQL := `UPDATE runtime_instances SET runtime_state='removed',removed_at=COALESCE(removed_at,$2),observed_at=$2,updated_at=now(),epoch=$3,revision=$4
         WHERE node_id=$1 AND runtime_id IS NOT NULL AND runtime_state!='removed' AND (epoch<$3 OR (epoch=$3 AND revision<=$4)) AND `
	if v.Complete && v.Revision > complete {
		if _, err = tx.Exec(ctx, removeSQL+`NOT (runtime_id=ANY($5::STRING[]))`, v.NodeID.String(), v.ObservedAt, v.Epoch, v.Revision, ids); err != nil {
			return err
		}
		complete = v.Revision
	}
	if len(v.Removed) > 0 {
		for _, id := range v.Removed {
			// A delete may precede the first received upsert. Persist its version
			// even for an unknown runtime ID so a delayed full snapshot cannot
			// insert it again. Existing identity/origin/associations stay intact.
			_, err = tx.Exec(ctx, `INSERT INTO runtime_instances
                (id,node_id,runtime_type,runtime_id,runtime_state,origin,observed_at,removed_at,epoch,revision)
                VALUES($1,$2,'docker',$3,'removed','observed',$4,$4,$5,$6)
                ON CONFLICT(node_id,runtime_type,runtime_id) DO UPDATE SET
                runtime_state='removed',removed_at=COALESCE(runtime_instances.removed_at,excluded.removed_at),
                observed_at=excluded.observed_at,updated_at=now(),epoch=excluded.epoch,revision=excluded.revision
                WHERE runtime_instances.epoch<excluded.epoch OR (runtime_instances.epoch=excluded.epoch AND runtime_instances.revision<excluded.revision)`, ObservedID(v.NodeID, "docker", id).String(), v.NodeID.String(), id, v.ObservedAt, v.Epoch, v.Revision)
			if err != nil {
				return err
			}
		}
	}
	if newer {
		_, err = tx.Exec(ctx, `UPDATE fabric_nodes SET name=$2,epoch=$3,revision=$4,complete_revision=$5,observed_at=$6,updated_at=now() WHERE id=$1`, v.NodeID.String(), v.NodeName, v.Epoch, v.Revision, complete, v.ObservedAt)
	} else {
		_, err = tx.Exec(ctx, `UPDATE fabric_nodes SET complete_revision=$2 WHERE id=$1`, v.NodeID.String(), complete)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
