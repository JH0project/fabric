package inventory

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/app"
	"github.com/JH0project/fabric/control/internal/database"
	"github.com/JH0project/fabric/control/internal/discovery"
	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/miekg/dns"
)

type labProcess struct {
	cmd  *exec.Cmd
	done chan error
	log  string
}

func migrateInventoryLab(ctx context.Context, s *Store) error {
	return (&database.Store{DB: s.DB}).Migrate(ctx)
}

func launch(t *testing.T, binary string, args ...string) *labProcess {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "process-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &labProcess{cmd: cmd, done: make(chan error, 1), log: f.Name()}
	go func() { err := cmd.Wait(); _ = f.Close(); p.done <- err }()
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			data, _ := os.ReadFile(p.log)
			if len(data) > 4000 {
				data = data[len(data)-4000:]
			}
			t.Log(string(data))
		}
	})
	return p
}
func (p *labProcess) stop() {
	if p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	p.cmd = nil
}
func unused6(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	return address
}
func eventually(t *testing.T, description string, fn func() bool) {
	t.Helper()
	end := time.Now().Add(25 * time.Second)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(description)
}
func certificates(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "inventory-lab-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"node", "root", "fabric_inventory"} {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(index + 2)), Subject: pkix.Name{CommonName: name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("::1")}}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &k.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		prefix := name
		if name != "node" {
			prefix = "client." + name
		}
		if err := os.WriteFile(filepath.Join(dir, prefix+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, prefix+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSecureInventoryIntegration(t *testing.T) {
	if os.Getenv("FABRIC_INVENTORY_LAB") != "1" {
		t.Skip("opt-in isolated secure IPv6 CRDB/NATS lab")
	}
	crdb := os.Getenv("FABRIC_LAB_CRDB_BINARY")
	natsBinary := os.Getenv("FABRIC_LAB_NATS_BINARY")
	if crdb == "" || natsBinary == "" {
		t.Fatal("lab binary paths required")
	}
	certs := certificates(t)
	dbAddress := unused6(t)
	httpAddress := unused6(t)
	brokerAddress := unused6(t)
	storeDir := filepath.Join(t.TempDir(), "db")
	dbArgs := []string{"start-single-node", "--listen-addr=" + dbAddress, "--advertise-addr=" + dbAddress, "--http-addr=" + httpAddress, "--certs-dir=" + certs, "--store=" + storeDir, "--cache=128MiB", "--max-sql-memory=256MiB", "--logtostderr=WARNING"}
	dbProcess := launch(t, crdb, dbArgs...)
	dsn := func(user string) string {
		u := url.URL{Scheme: "postgresql", User: url.User(user), Host: dbAddress, Path: "/defaultdb"}
		q := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {filepath.Join(certs, "ca.crt")}, "sslcert": {filepath.Join(certs, "client."+user+".crt")}, "sslkey": {filepath.Join(certs, "client."+user+".key")}}
		u.RawQuery = q.Encode()
		return u.String()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var admin *Store
	var err error
	eventually(t, "secure CRDB did not become ready", func() bool {
		c, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		admin, err = OpenStore(c, dsn("root"))
		return err == nil
	})
	defer admin.Close()
	if err := migrateInventoryLab(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if err := migrateInventoryLab(ctx, admin); err != nil {
		t.Fatal("non-idempotent migration", err)
	}
	testNodeRegistry(t, ctx, dsn("root"))
	if _, err := admin.Pool.ExecContext(ctx, `REVOKE ALL ON SCHEMA public FROM public; CREATE USER fabric_inventory; GRANT CONNECT ON DATABASE defaultdb TO fabric_inventory; GRANT USAGE ON SCHEMA public TO fabric_inventory; GRANT SELECT,INSERT,UPDATE ON TABLE node_observations,runtime_instances TO fabric_inventory; GRANT SELECT ON TABLE nodes,runtime_instance_observations TO fabric_inventory`); err != nil {
		t.Fatal(err)
	}
	wrongCA := certificates(t)
	wrongURL, err := url.Parse(dsn("root"))
	if err != nil {
		t.Fatal(err)
	}
	wrongQuery := wrongURL.Query()
	wrongQuery.Set("sslrootcert", filepath.Join(wrongCA, "ca.crt"))
	wrongURL.RawQuery = wrongQuery.Encode()
	badStore, err := OpenStore(ctx, wrongURL.String())
	if err == nil {
		badStore.Close()
		t.Fatal("untrusted database CA accepted")
	}
	store, err := OpenStore(ctx, dsn("fabric_inventory"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	labPassword := uuid.NewString()
	if _, err := admin.Pool.ExecContext(ctx, `CREATE USER fabric_password WITH PASSWORD $1`, labPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.ExecContext(ctx, `GRANT CONNECT ON DATABASE defaultdb TO fabric_password; GRANT USAGE ON SCHEMA public TO fabric_password; GRANT SELECT,INSERT,UPDATE ON TABLE node_observations,runtime_instances TO fabric_password`); err != nil {
		t.Fatal(err)
	}
	passwordURL, err := url.Parse(dsn("fabric_inventory"))
	if err != nil {
		t.Fatal(err)
	}
	passwordURL.User = url.UserPassword("fabric_password", labPassword)
	passwordQuery := passwordURL.Query()
	passwordQuery.Del("sslcert")
	passwordQuery.Del("sslkey")
	passwordURL.RawQuery = passwordQuery.Encode()
	passwordStore, err := OpenStore(ctx, passwordURL.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := passwordStore.Pool.PingContext(ctx); err != nil {
		t.Fatal("secure password database login failed")
	}
	passwordStore.Close()
	node := uuid.New()
	seedNode := func(id uuid.UUID, sequence int64) {
		t.Helper()
		if err := (&database.Store{DB: admin.DB}).ImportNodes(ctx, []database.Node{{UUID: id, ID: sequence, Region: "lab", Zone: "lab-1"}}); err != nil {
			t.Fatal(err)
		}
	}
	seedNode(node, 1)
	count := func(where string) int {
		t.Helper()
		var n int
		if err := admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM runtime_instances WHERE node_id=$1 AND `+where, node.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	one := fixture(node, 1, true, "a", "b")
	if err := store.Apply(ctx, one); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(ctx, one); err != nil {
		t.Fatal(err)
	}
	if count("true") != 2 {
		t.Fatal("duplicate identities")
	}
	readback, err := (&database.Store{DB: store.DB}).RuntimeInstances(ctx, node)
	if err != nil || len(readback) != 2 {
		t.Fatal("GORM runtime observation read", readback, err)
	}
	for _, record := range readback {
		if record.RuntimeID == nil || record.ID != ObservedID(node, "docker", *record.RuntimeID) || record.ObservationStale || record.Labels["fabric.discovery.service"] != "api" || len(record.NetworkAttachments["mesh"]) != 2 {
			t.Fatal("runtime identity, JSON metadata or freshness did not round-trip", record)
		}
	}
	stop := fixture(node, 2, false, "a", "b")
	stop.Instances[0].RuntimeState = "exited"
	if err := store.Apply(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if count("runtime_state='exited'") != 1 {
		t.Fatal("stop state not persisted")
	}
	removed := fixture(node, 3, false, "a")
	removed.Removed = []string{"b"}
	if err := store.Apply(ctx, removed); err != nil {
		t.Fatal(err)
	}
	recreated := fixture(node, 4, true, "a", "c")
	if err := store.Apply(ctx, recreated); err != nil {
		t.Fatal(err)
	}
	if count("true") != 3 || count("runtime_state='removed'") != 1 {
		t.Fatal("removal history or recreation lost")
	}
	ghost := fixture(node, 5, false, "a", "c", "ghost")
	if err := store.Apply(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	newest := fixture(node, 7, false, "a", "c")
	newest.Instances[0].Image = "example:newest"
	if err := store.Apply(ctx, newest); err != nil {
		t.Fatal(err)
	}
	late := fixture(node, 6, true, "a", "c")
	if err := store.Apply(ctx, late); err != nil {
		t.Fatal(err)
	}
	if count("image='example:newest'") != 1 || count("runtime_id='ghost' AND runtime_state='removed'") != 1 {
		t.Fatal("late complete inventory damaged newer state or failed reconciliation")
	}
	if err := store.Apply(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	if count("runtime_id='ghost' AND runtime_state='removed'") != 1 {
		t.Fatal("stale snapshot resurrected removal")
	}
	managedID, serviceID := uuid.New(), uuid.New()
	if _, err := admin.Pool.ExecContext(ctx, `INSERT INTO runtime_instances(id,node_id,service_id,runtime_type,runtime_id,origin,observed_at,epoch,revision) VALUES($1,$2,$3,'docker','managed-runtime','fabric_managed',now(),0,0)`, managedID.String(), node.String(), serviceID.String()); err != nil {
		t.Fatal(err)
	}
	futureID := uuid.New()
	if _, err := admin.Pool.ExecContext(ctx, `INSERT INTO runtime_instances(id,node_id,runtime_type,origin,observed_at,epoch,revision) VALUES($1,$2,'docker','fabric_managed',now(),0,0)`, futureID.String(), node.String()); err != nil {
		t.Fatal(err)
	}
	managedObservation := fixture(node, 8, true, "a", "c", "managed-runtime")
	if err := store.Apply(ctx, managedObservation); err != nil {
		t.Fatal(err)
	}
	var actualID, actualService, origin string
	if err := admin.Pool.QueryRowContext(ctx, `SELECT id::STRING,service_id::STRING,origin FROM runtime_instances WHERE node_id=$1 AND runtime_id='managed-runtime'`, node.String()).Scan(&actualID, &actualService, &origin); err != nil {
		t.Fatal(err)
	}
	if actualID != managedID.String() || actualService != serviceID.String() || origin != "fabric_managed" {
		t.Fatal("existing managed identity/associations overwritten")
	}
	if count("runtime_id IS NULL AND removed_at IS NULL") != 1 {
		t.Fatal("future unbound allocation falsely removed")
	}
	// Freshness is based on database receipt time, not a publisher's clock.
	if _, err := admin.Pool.ExecContext(ctx, `UPDATE node_observations SET observed_at=now()+INTERVAL '1 day',updated_at=now()-INTERVAL '2 minutes' WHERE node_id=$1`, node.String()); err != nil {
		t.Fatal(err)
	}
	var stale bool
	var state string
	if err := admin.Pool.QueryRowContext(ctx, `SELECT observation_stale,runtime_state FROM runtime_instance_observations WHERE node_id=$1 AND runtime_id='a'`, node.String()).Scan(&stale, &state); err != nil {
		t.Fatal(err)
	}
	if !stale || state != "running" {
		t.Fatal("node outage fabricated stopped containers")
	}
	staleReadback, err := (&database.Store{DB: store.DB}).RuntimeInstances(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range staleReadback {
		if !record.ObservationStale {
			t.Fatal("GORM read lost stale-node status")
		}
	}
	for _, query := range []string{"CREATE TABLE forbidden_runtime(id INT)", "DELETE FROM runtime_instances WHERE false", "INSERT INTO nodes(uuid,id,region,zone) VALUES(gen_random_uuid(),65535,'forbidden','forbidden')", "SELECT * FROM schema_migrations"} {
		_, err := store.Pool.ExecContext(ctx, query)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Fatal("consumer privilege escaped", query, err)
		}
	}
	// An agent restart starts a new epoch. Old rows become stale until a complete
	// inventory arrives; an old-epoch message with a large revision is still old.
	nextEpoch := fixture(node, 1, false, "a")
	nextEpoch.Epoch = 2
	if err := store.Apply(ctx, nextEpoch); err != nil {
		t.Fatal(err)
	}
	oldEpoch := fixture(node, 100, true, "a", "must-not-appear")
	if err := store.Apply(ctx, oldEpoch); err != nil {
		t.Fatal(err)
	}
	if count("runtime_id='must-not-appear'") != 0 {
		t.Fatal("old epoch overrode restarted publisher")
	}
	if err := admin.Pool.QueryRowContext(ctx, `SELECT observation_stale,runtime_state FROM runtime_instance_observations WHERE node_id=$1 AND runtime_id='c'`, node.String()).Scan(&stale, &state); err != nil || !stale || state != "running" {
		t.Fatal("incomplete new epoch fabricated removal", stale, state, err)
	}
	fullEpoch := fixture(node, 2, true, "a")
	fullEpoch.Epoch = 2
	if err := store.Apply(ctx, fullEpoch); err != nil {
		t.Fatal(err)
	}
	if count("runtime_id='c' AND runtime_state='removed'") != 1 || count("runtime_id IS NULL AND removed_at IS NULL") != 1 {
		t.Fatal("new epoch failed reconciliation or removed an unbound allocation")
	}
	// Duplicate consumers may race on the first snapshot for a node. Their
	// transactions must converge on one row and one publisher watermark.
	concurrentNode := uuid.New()
	seedNode(concurrentNode, 5)
	concurrentSnapshot := fixture(concurrentNode, 1, true, "concurrent-runtime")
	var writers sync.WaitGroup
	writeErrors := make(chan error, 4)
	for range 4 {
		writers.Add(1)
		go func() { defer writers.Done(); writeErrors <- store.Apply(ctx, concurrentSnapshot) }()
	}
	writers.Wait()
	close(writeErrors)
	for err := range writeErrors {
		if err != nil {
			t.Fatal("concurrent snapshot persistence", err)
		}
	}
	var concurrentCount int
	if err := admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM runtime_instances WHERE node_id=$1`, concurrentNode.String()).Scan(&concurrentCount); err != nil || concurrentCount != 1 {
		t.Fatal("concurrent consumers duplicated runtime identity", concurrentCount, err)
	}
	unknownNode := uuid.New()
	deleteFirst := fixture(unknownNode, 3, false)
	deleteFirst.NodeName = "node-two"
	deleteFirst.Removed = []string{"not-seen-before", "another-unseen"}
	if err := store.Apply(ctx, deleteFirst); err == nil {
		t.Fatal("unregistered node observation accepted")
	}
	seedNode(unknownNode, 2)
	if err := store.Apply(ctx, deleteFirst); err != nil {
		t.Fatal(err)
	}
	lateFirst := fixture(unknownNode, 2, true, "not-seen-before")
	lateFirst.NodeName = "node-two"
	if err := store.Apply(ctx, lateFirst); err != nil {
		t.Fatal(err)
	}
	if err := admin.Pool.QueryRowContext(ctx, `SELECT runtime_state FROM runtime_instances WHERE node_id=$1 AND runtime_id='not-seen-before'`, unknownNode.String()).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removed" {
		t.Fatal("late first snapshot resurrected an unseen removal")
	}
	batchNode := uuid.New()
	seedNode(batchNode, 6)
	batchIDs := make([]string, 256)
	for i := range batchIDs {
		batchIDs[i] = fmt.Sprintf("batch-%03d", i)
	}
	batch := fixture(batchNode, 1, true, batchIDs...)
	if err := store.Apply(ctx, batch); err != nil {
		t.Fatal("large inventory batch", err)
	}
	if err := store.Apply(ctx, batch); err != nil {
		t.Fatal("large batch replay", err)
	}
	var batchCount int
	if err := admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM runtime_instances WHERE node_id=$1`, batchNode.String()).Scan(&batchCount); err != nil || batchCount != 256 {
		t.Fatal("large inventory batch lost or duplicated rows", batchCount, err)
	}
	if err := store.Apply(ctx, fixture(batchNode, 2, true)); err != nil {
		t.Fatal("empty inventory reconciliation", err)
	}
	if err := admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM runtime_instances WHERE node_id=$1 AND runtime_state='removed'`, batchNode.String()).Scan(&batchCount); err != nil || batchCount != 256 {
		t.Fatal("large batch removal history", batchCount, err)
	}

	_, port, err := net.SplitHostPort(brokerAddress)
	if err != nil {
		t.Fatal(err)
	}
	conf := fmt.Sprintf("host: \"::1\"\nport: %s\ntls { cert_file: %q, key_file: %q, ca_file: %q, verify: true }\n", port, filepath.Join(certs, "node.crt"), filepath.Join(certs, "node.key"), filepath.Join(certs, "ca.crt"))
	configPath := filepath.Join(t.TempDir(), "nats.conf")
	if err := os.WriteFile(configPath, []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	broker := launch(t, natsBinary, "-c", configPath)
	passwordBrokerAddress := unused6(t)
	_, passwordPort, err := net.SplitHostPort(passwordBrokerAddress)
	if err != nil {
		t.Fatal(err)
	}
	passwordConfig := fmt.Sprintf("host: \"::1\"\nport: %s\nauthorization { user: \"fabric_lab\", password: %q }\ntls { cert_file: %q, key_file: %q, ca_file: %q }\n", passwordPort, labPassword, filepath.Join(certs, "node.crt"), filepath.Join(certs, "node.key"), filepath.Join(certs, "ca.crt"))
	passwordConfigPath := filepath.Join(t.TempDir(), "password-nats.conf")
	if err := os.WriteFile(passwordConfigPath, []byte(passwordConfig), 0600); err != nil {
		t.Fatal(err)
	}
	_ = launch(t, natsBinary, "-c", passwordConfigPath)
	passwordBus, err := NewBusWithAuth("tls://"+passwordBrokerAddress, "lab", TLSFiles{CA: filepath.Join(certs, "ca.crt")}, NATSAuth{Username: "fabric_lab", Password: labPassword})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(passwordBus.Close)
	eventually(t, "TLS/password NATS client did not connect", func() bool { return passwordBus.conn.IsConnected() })
	passwordBus.Close()
	files := TLSFiles{CA: filepath.Join(certs, "ca.crt"), Certificate: filepath.Join(certs, "client.fabric_inventory.crt"), Key: filepath.Join(certs, "client.fabric_inventory.key")}
	newBus := func() *Bus {
		t.Helper()
		b, err := NewBus("tls://"+brokerAddress, "lab", files)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	}
	aBus, bBus, pBus := newBus(), newBus(), newBus()
	eventually(t, "NATS TLS clients did not connect", func() bool { return aBus.conn.IsConnected() && bBus.conn.IsConnected() && pBus.conn.IsConnected() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sourceNode := uuid.New()
	receiverNode := uuid.New()
	seedNode(sourceNode, 3)
	seedNode(receiverNode, 4)
	cache, err := discovery.NewCache("fabric.internal", "receiver")
	if err != nil {
		t.Fatal(err)
	}
	dnsCtx, stopDNS := context.WithCancel(ctx)
	dnsDone := make(chan error, 1)
	dnsReady := make(chan string, 1)
	dnsServer := &discovery.DNS{Cache: cache, TTL: 5, Address: "[::1]:0", IPv6Only: true, OnListening: func(address string) { dnsReady <- address }}
	go func() { dnsDone <- dnsServer.Run(dnsCtx) }()
	t.Cleanup(func() {
		stopDNS()
		select {
		case err := <-dnsDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("DNS did not stop")
		}
	})
	var dnsAddress string
	select {
	case dnsAddress = <-dnsReady:
	case <-time.After(5 * time.Second):
		t.Fatal("DNS did not start")
	}
	queryDNS := func(qtype uint16) *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion("api.service.fabric.internal.", qtype)
		r, _, err := (&dns.Client{Net: "udp6", Timeout: time.Second}).Exchange(m, dnsAddress)
		if err != nil {
			return nil
		}
		return r
	}
	a := NewAgent(sourceNode, "source", 1, aBus, logger, nil)
	b := NewAgent(receiverNode, "receiver", 1, bBus, logger, cache)
	p := &Persistence{Bus: pBus, Store: store, Logger: logger, Pending: NewMailbox()}
	runCtx, stopAll := context.WithCancel(ctx)
	defer stopAll()
	done := make(chan error, 3)
	go func() { done <- app.New(a).Run(runCtx) }()
	go func() { done <- app.New(b).Run(runCtx) }()
	go func() { done <- app.New(p).Run(runCtx) }()
	t.Cleanup(func() {
		stopAll()
		for range 3 {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("workers not joined")
			}
		}
	})
	eventually(t, "subscribers not established", func() bool {
		return aBus.conn.NumSubscriptions() >= 2 && bBus.conn.NumSubscriptions() >= 2 && pBus.conn.NumSubscriptions() >= 1
	})
	c := runtime.Container{ID: "live-a", Name: "workload", Image: "example:v1", State: "running", Labels: map[string]string{"fabric.discovery.service": "api", "fabric.discovery.network": "mesh", "fabric.discovery.scope": "global"}, Networks: map[string][]string{"mesh": {"fd00::1234", "10.0.0.1"}}}
	a.Observe(runtime.Observation{Containers: []runtime.Container{c, {ID: "unlabelled", State: "running"}}, Complete: true})
	eventually(t, "peer discovery did not receive state", func() bool { return len(b.Peers.Endpoints(time.Now())) == 1 })
	eventually(t, "remote inventory did not reach DNS", func() bool {
		r := queryDNS(dns.TypeAAAA)
		return r != nil && len(r.Answer) == 1 && r.Answer[0].(*dns.AAAA).AAAA.String() == "fd00::1234"
	})
	if r := queryDNS(dns.TypeA); r == nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatal("IPv4 leaked into synchronized DNS")
	}

	persisted := func(id string, expected string) bool {
		var state string
		c, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		err := store.Pool.QueryRowContext(c, `SELECT runtime_state FROM runtime_instances WHERE node_id=$1 AND runtime_id=$2`, sourceNode.String(), id).Scan(&state)
		return err == nil && state == expected
	}
	eventually(t, "unlabelled inventory not persisted", func() bool { return persisted("unlabelled", "running") })
	eventually(t, "labelled inventory not persisted", func() bool { return persisted("live-a", "running") })

	// Stop the actual database. Observation and NATS must continue independently.
	dbProcess.stop()
	c.State = "exited"
	a.Observe(runtime.Observation{Containers: []runtime.Container{c}})
	eventually(t, "DB outage blocked live peer update", func() bool { return len(b.Peers.Endpoints(time.Now())) == 0 })
	c.State = "running"
	c.Image = "example:while-db-down"
	a.Observe(runtime.Observation{Containers: []runtime.Container{c}, Complete: true})
	eventually(t, "DB outage blocked live peer recovery", func() bool { return len(b.Peers.Endpoints(time.Now())) == 1 })
	dbProcess = launch(t, crdb, dbArgs...)
	eventually(t, "persistence did not recover after DB restart", func() bool { return persisted("live-a", "running") })
	eventually(t, "latest state lost during DB outage", func() bool {
		var image string
		c, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		return store.Pool.QueryRowContext(c, `SELECT image FROM runtime_instances WHERE node_id=$1 AND runtime_id='live-a'`, sourceNode.String()).Scan(&image) == nil && image == "example:while-db-down"
	})

	// Lose a removal while the broker is down, then repair with a complete list.
	broker.stop()
	a.Observe(runtime.Observation{Containers: nil, Complete: true})
	if a.pending()[0].Complete != true {
		t.Fatal("full repair snapshot not retained")
	}
	broker = launch(t, natsBinary, "-c", configPath)
	eventually(t, "NATS did not reconnect", func() bool { return aBus.conn.IsConnected() && bBus.conn.IsConnected() && pBus.conn.IsConnected() })
	eventually(t, "complete inventory did not repair missed removals", func() bool { return persisted("live-a", "removed") && len(b.Peers.Endpoints(time.Now())) == 0 })
	_ = broker
	_ = dbProcess
	t.Log("secure IPv6 CRDB/NATS: identity, history, managed compatibility, ordering, DB outage, broker outage and reconciliation passed")
}

func TestTransportRejectsUnsafeConfig(t *testing.T) {
	if _, err := NewBus("nats://[::1]:4222", "fabric", TLSFiles{}); err == nil {
		t.Fatal("plaintext NATS accepted")
	}
	if _, err := NewBus("tls://127.0.0.1:4222", "fabric", TLSFiles{}); err == nil {
		t.Fatal("IPv4 NATS accepted")
	}
	if _, err := OpenStore(context.Background(), "postgresql://127.0.0.1/db?sslmode=verify-full&sslrootcert=x&sslcert=y&sslkey=z"); err == nil {
		t.Fatal("IPv4 DB accepted")
	}
	if _, err := OpenStore(context.Background(), "postgresql://[::1]/db?sslmode=require"); err == nil {
		t.Fatal("non-verifying TLS accepted")
	}
}
