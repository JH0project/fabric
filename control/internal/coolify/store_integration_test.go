package coolify

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/JH0project/fabric/control/internal/database"
	"github.com/JH0project/fabric/control/internal/environment"
)

// Synthetic ciphertext; actual Laravel-generated fixtures separately prove format parity.
func encryptedTestValue(text string, serialized bool) string {
	if serialized {
		text = fmt.Sprintf("s:%d:\"%s\";", len(text), text)
	}
	key := []byte(strings.Repeat("k", 32))
	iv := []byte(strings.Repeat("i", 16))
	block, _ := aes.NewCipher(key)
	plain := []byte(text)
	padding := 16 - len(plain)%16
	for range padding {
		plain = append(plain, byte(padding))
	}
	value := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(value, plain)
	iv64 := base64.StdEncoding.EncodeToString(iv)
	value64 := base64.StdEncoding.EncodeToString(value)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(iv64 + value64))
	payload, _ := json.Marshal(map[string]string{"iv": iv64, "value": value64, "mac": hex.EncodeToString(mac.Sum(nil)), "tag": ""})
	return base64.StdEncoding.EncodeToString(payload)
}

func TestStoreCurrentCoolifySchema(t *testing.T) {
	dsn := os.Getenv("FABRIC_TEST_COOLIFY_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable TLS/IPv6 PostgreSQL with current Coolify schema")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/fabric_coolify_test" {
		t.Fatal("integration database must be fabric_coolify_test")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// This test requires the current external schema, not a hand-maintained imitation.
	var columns int64
	if db.DB.Raw("SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='applications'").Scan(&columns).Error != nil || columns < 90 {
		t.Fatal("current Coolify application schema is required")
	}
	statements := []string{
		`INSERT INTO teams(id,name) VALUES (99001,'fixture-team'),(99002,'foreign-team')`,
		`INSERT INTO projects(id,uuid,name,team_id) VALUES (99001,'fixture-project','fixture',99001)`,
		`INSERT INTO environments(id,uuid,name,project_id) VALUES (99001,'fixture-environment','fixture',99001)`,
		`INSERT INTO servers(id,uuid,name,ip,team_id,private_key_id) VALUES (99001,'fixture-node-1','node1','::1',99001,0),(99002,'fixture-node-2','node2','::1',99001,0),(99003,'fixture-node-3','node3','::1',99001,0),(99004,'fixture-node-4','foreign','::1',99002,0)`,
		`INSERT INTO standalone_dockers(id,uuid,name,network,server_id) VALUES (99001,'fixture-destination','fixture','fixture',99001)`,
		`INSERT INTO applications(id,uuid,name,git_repository,git_branch,build_pack,ports_exposes,environment_id,destination_type,destination_id) VALUES (99001,'fixture-app','fixture','fixture','main','dockerfile','8080',99001,'App\Models\StandaloneDocker',99001)`,
		`INSERT INTO additional_destinations(application_id,server_id,standalone_docker_id) VALUES (99001,99002,99001),(99001,99004,99001)`,
	}
	for _, q := range statements {
		if err := db.DB.Exec(q).Error; err != nil {
			t.Fatal("fixture insert failed", err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM environment_variables WHERE resourceable_id=99001`, `DELETE FROM shared_environment_variables WHERE team_id IN (99001,99002)`,
			`DELETE FROM additional_destinations WHERE application_id=99001`, `DELETE FROM applications WHERE id=99001`,
			`DELETE FROM standalone_dockers WHERE id=99001`, `DELETE FROM servers WHERE id BETWEEN 99001 AND 99004`,
			`DELETE FROM environments WHERE id=99001`, `DELETE FROM projects WHERE id=99001`, `DELETE FROM teams WHERE id IN (99001,99002)`,
		} {
			if e := db.DB.Exec(q).Error; e != nil {
				t.Error("fixture cleanup failed", e)
			}
		}
	})
	for _, v := range []struct {
		key, text        string
		runtime, preview bool
	}{
		{"MESSAGE", "{{team.NAME}}/{{project.NAME}}/{{environment.NAME}}/{{server.REGION}}", true, false},
		{"PLAIN", "  {\"value\":\"$literal\"}\n ", true, false}, {"ZERO", "0", true, false},
		{"PREVIEW", "hidden", true, true}, {"BUILD", "hidden", false, false}, {"NIXPACKS_BUILD_CMD", "hidden", true, false},
	} {
		if db.DB.Exec(`INSERT INTO environment_variables(uuid,key,value,resourceable_type,resourceable_id,is_runtime,is_preview) VALUES (?,?,?,'App\Models\Application',99001,?,?)`, "fixture-"+v.key, v.key, encryptedTestValue(v.text, true), v.runtime, v.preview).Error != nil {
			t.Fatal("variable insert failed")
		}
	}
	for _, v := range []struct {
		scope, key, text string
		id, team         int64
	}{
		{"team", "NAME", "team", 99001, 99001}, {"project", "NAME", "project", 99001, 99001},
		{"environment", "NAME", "environment", 99001, 99001}, {"server", "REGION", "one", 99001, 99001},
		{"server", "REGION", "two", 99002, 99001}, {"server", "REGION", "foreign", 99001, 99002},
	} {
		row := map[string]any{"type": v.scope, "key": v.key, "value": encryptedTestValue(v.text, false), "team_id": v.team}
		row[v.scope+"_id"] = v.id
		if db.DB.Table("public.shared_environment_variables").Create(row).Error != nil {
			t.Fatal("shared insert failed")
		}
	}
	for _, id := range []int64{99001, 99002} {
		cfg := Config{Host: u.Hostname(), Port: u.Port(), Name: "fabric_coolify_test", Username: u.User.Username(), CA: u.Query().Get("sslrootcert"), AppKeys: []string{"base64:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}, ServerID: id}
		cfg.Password, _ = u.User.Password()
		store, e := Open(ctx, cfg)
		if e != nil {
			t.Fatal(e)
		}
		resolver := environment.Resolver{Store: store}
		got, e := resolver.Resolve(ctx, environment.Reference{ApplicationUUID: "fixture-app", ServerID: id, TeamID: 99001})
		region := "one"
		if id == 99002 {
			region = "two"
		}
		want := []environment.Variable{{Key: "MESSAGE", Value: "team/project/environment/" + region}, {Key: "PLAIN", Value: `{"value":"$literal"}`}, {Key: "ZERO", Value: "0"}}
		if e != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("snapshot resolution failed", e, got)
		}
		for _, ref := range []environment.Reference{{ApplicationUUID: "fixture-app", ServerID: id, TeamID: 99002}, {ApplicationUUID: "missing", ServerID: id, TeamID: 99001}, {ApplicationUUID: "fixture-app", ServerID: 99003, TeamID: 99001}, {ApplicationUUID: "fixture-app", ServerID: 99004, TeamID: 99001}} {
			if _, e := resolver.Resolve(ctx, ref); e == nil {
				t.Fatal("unauthorized reference accepted")
			}
		}
		if e := store.database.DB.Exec(`UPDATE public.applications SET name='must-not-write' WHERE id=99001`).Error; e == nil {
			t.Fatal("read-only adapter allowed a write")
		}
		store.Close()
	}
	if db.DB.Exec(`UPDATE environment_variables SET value='invalid-ciphertext' WHERE resourceable_id=99001 AND key='MESSAGE'`).Error != nil {
		t.Fatal("tamper fixture failed")
	}
	// A server-local adapter must still reject a nonmember or foreign-team server
	// even when its configured identity matches the requested server.
	decryptor, _ := newDecryptor([]string{"base64:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}, "AES-256-CBC")
	for _, id := range []int64{99003, 99004} {
		store := &Store{database: db, decryptor: decryptor, serverID: id}
		if _, e := store.Load(ctx, environment.Reference{ApplicationUUID: "fixture-app", ServerID: id, TeamID: 99001}); e == nil {
			t.Fatal("nonmember/foreign-team destination accepted")
		}
	}
	store := &Store{database: db, decryptor: decryptor, serverID: 99001}
	if _, e := store.Load(ctx, environment.Reference{ApplicationUUID: "fixture-app", ServerID: 99001, TeamID: 99001}); e == nil {
		t.Fatal("invalid ciphertext accepted")
	}
}
