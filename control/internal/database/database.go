// Package database owns Fabric's versioned relational schema and node registry.
package database

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Store struct{ DB *gorm.DB }

// Open requires verified TLS and uses IPv6 for every connection, including DNS
// names. It never applies migrations implicitly or logs connection credentials.
func Open(ctx context.Context, dsn string) (*Store, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Query().Get("sslmode") != "verify-full" || u.Query().Get("sslrootcert") == "" {
		return nil, errors.New("database URL requires verify-full and a CA path")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.To4() != nil {
		return nil, errors.New("database endpoint must use IPv6")
	}
	if (u.Query().Get("sslcert") == "") != (u.Query().Get("sslkey") == "") {
		return nil, errors.New("database client certificate and key must be supplied together")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	if cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify {
		return nil, errors.New("database TLS verification is required")
	}
	cfg.Fallbacks = nil
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams["application_name"] = "fabric"
	cfg.DialFunc = func(ctx context.Context, _ string, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp6", addr)
	}
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxLifetime(30 * time.Minute)
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, errors.New("database connection failed")
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableAutomaticPing: true})
	if err != nil {
		pool.Close()
		return nil, errors.New("initialize database ORM failed")
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error {
	db, err := s.DB.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

// retry restarts the complete transaction on CockroachDB serialization failures.
func retry(ctx context.Context, fn func() error) error {
	for attempt := 0; attempt < 5; attempt++ {
		err := fn()
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "40001" {
			return err
		}
		t := time.NewTimer(time.Duration(10*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return errors.New("database transaction serialization retries exhausted")
}
