package inventory

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/JH0project/fabric/control/internal/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Uses the existing opt-in secure CRDB fixture, but a separate database. This
// verifies an upgrade from the already deployed nodes-only schema.
func testNodeRegistry(t *testing.T, ctx context.Context, rootDSN string) {
	t.Helper()
	admin, err := database.Open(ctx, rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.DB.Exec("CREATE DATABASE fabric_nodes_lab").Error; err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(rootDSN)
	u.Path = "/fabric_nodes_lab"
	s, err := database.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.MigrateTo(ctx, database.NodesMigration); err != nil {
		t.Fatal("GORM nodes migration", err)
	}
	if err := s.MigrateTo(ctx, database.NodesMigration); err != nil {
		t.Fatal("migration replay", err)
	}
	var columns int64
	if err := s.DB.Raw("SELECT count(*) FROM information_schema.columns WHERE table_name='nodes' AND table_schema='public'").Scan(&columns).Error; err != nil || columns != 4 {
		t.Fatal("nodes must have exactly four columns", columns, err)
	}
	var versions int64
	if err := s.DB.Table("schema_migrations").Count(&versions).Error; err != nil || versions != 1 {
		t.Fatal("migration history", versions, err)
	}
	n := database.Node{UUID: uuid.New(), ID: 2, Region: "jp", Zone: "jp-1"}
	if err := s.ImportNodes(ctx, []database.Node{n}); err != nil {
		t.Fatal(err)
	}
	n.Zone = "jp-3"
	if err := s.ImportNodes(ctx, []database.Node{n}); err != nil {
		t.Fatal("idempotent placement refresh", err)
	}
	conflict := n
	conflict.UUID = uuid.New()
	newNode := database.Node{UUID: uuid.New(), ID: 3, Region: "jp", Zone: "jp-3"}
	if err := s.ImportNodes(ctx, []database.Node{newNode, conflict}); err == nil {
		t.Fatal("identity remapping accepted")
	}
	nodes, err := s.Nodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0] != n {
		t.Fatal("import was not atomic", nodes, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("upgrade existing node registry", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("runtime migration replay", err)
	}
	upgraded, err := s.Nodes(ctx)
	if err != nil || len(upgraded) != 1 || upgraded[0] != n {
		t.Fatal("runtime migration changed node identity", upgraded, err)
	}
	if err := s.DB.Table("schema_migrations").Count(&versions).Error; err != nil || versions != 2 {
		t.Fatal("runtime migration history", versions, err)
	}
	if err := s.DB.Raw("SELECT count(*) FROM information_schema.columns WHERE table_name='nodes' AND table_schema='public'").Scan(&columns).Error; err != nil || columns != 4 {
		t.Fatal("runtime migration expanded nodes model", columns, err)
	}
	for _, sql := range []string{
		"REVOKE ALL ON DATABASE fabric_nodes_lab FROM public",
		"REVOKE ALL ON SCHEMA public FROM public",
		"CREATE USER fabric_nodes_app WITH PASSWORD 'isolated-test-password'",
		"GRANT CONNECT ON DATABASE fabric_nodes_lab TO fabric_nodes_app",
		"GRANT USAGE ON SCHEMA public TO fabric_nodes_app",
		"GRANT SELECT, INSERT, UPDATE ON TABLE nodes TO fabric_nodes_app",
	} {
		if err := s.DB.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	u.User = url.UserPassword("fabric_nodes_app", "isolated-test-password")
	q := u.Query()
	q.Del("sslcert")
	q.Del("sslkey")
	u.RawQuery = q.Encode()
	app, err := database.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.Nodes(ctx); err != nil {
		t.Fatal("runtime select denied", err)
	}
	if err := app.ImportNodes(ctx, []database.Node{n, newNode}); err != nil {
		t.Fatal("runtime insert/update denied", err)
	}
	for _, sql := range []string{"CREATE TABLE forbidden(id INT)", "DELETE FROM nodes WHERE id=2", "SELECT * FROM schema_migrations", "SELECT * FROM defaultdb.public.nodes"} {
		err := app.DB.Exec(sql).Error
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Fatal("runtime privilege check did not deny access", sql, err)
		}
	}
	if err := s.DB.Exec("INSERT INTO schema_migrations(id) VALUES ('unknown_future_migration')").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("unknown migration accepted")
	}
	t.Log("GORM nodes: migration replay, four-column schema, identity preservation, atomic import and runtime privilege isolation passed")
}
