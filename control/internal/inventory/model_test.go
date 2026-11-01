package inventory

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/google/uuid"
)

func fixture(node uuid.UUID, revision uint64, complete bool, ids ...string) Snapshot {
	var containers []runtime.Container
	for _, id := range ids {
		containers = append(containers, runtime.Container{ID: id, Name: "same-name", Image: "example:v1", ImageID: "sha256:test", State: "running", Health: "healthy", Labels: map[string]string{"fabric.discovery.service": "api", "fabric.discovery.network": "mesh", "fabric.discovery.scope": "global"}, Networks: map[string][]string{"mesh": {"fd00::42", "10.0.0.1"}}})
	}
	return Project(node, "node-one", 1, revision, runtime.Observation{Containers: containers, Complete: complete}, time.Now())
}

func TestPhysicalIdentityAndMetadata(t *testing.T) {
	node := uuid.New()
	a := fixture(node, 1, true, "docker-a")
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	i := a.Instances[0]
	b := fixture(node, 2, true, "docker-a", "docker-b")
	if i.ID != b.Instances[0].ID || i.ID == b.Instances[1].ID || i.ID == ObservedID(uuid.New(), "docker", "docker-a") {
		t.Fatal("identity reused or changed")
	}
	i.RuntimeState = "exited"
	if i.ID != ObservedID(node, "docker", i.RuntimeID) {
		t.Fatal("stop changed UUID")
	}
	c := runtime.Container{ID: "a", State: "running", Labels: map[string]string{"secret": "do-not-export", "fabric.discovery.service": "api", "fabric.instance.id": uuid.NewString()}}
	s := Project(node, "node-one", 1, 3, runtime.Observation{Containers: []runtime.Container{c}}, time.Now())
	if len(s.Instances[0].Labels) != 1 || s.Instances[0].Origin != "observed" {
		t.Fatal("label leaked or container adopted")
	}
	s.Instances[0].Origin = "fabric_managed"
	if s.Validate() == nil {
		t.Fatal("untrusted management authority accepted")
	}
}

func TestPeersOrderingReconciliationAndExpiry(t *testing.T) {
	node := uuid.New()
	peers := NewPeers(uuid.New(), time.Minute)
	s := fixture(node, 1, true, "a", "old")
	peers.Apply(s)
	newer := fixture(node, 3, false, "a", "new")
	newer.Removed = []string{"old"}
	peers.Apply(newer)
	late := fixture(node, 2, true, "a")
	peers.Apply(late)
	if got := peers.Endpoints(time.Now()); len(got) != 2 {
		t.Fatalf("late full removed newer instance or resurrected old: %v", got)
	}
	for _, e := range peers.Endpoints(time.Now()) {
		if e.ID == "old" || len(e.Addresses) != 1 || e.Addresses[0] != "fd00::42" {
			t.Fatal("removed or IPv4 endpoint leaked")
		}
	}
	peers.Apply(s)
	if len(peers.Endpoints(time.Now())) != 2 {
		t.Fatal("old snapshot overwrote new")
	}
	if len(peers.Endpoints(time.Now().Add(2*time.Minute))) != 0 {
		t.Fatal("stale remote endpoints stayed live")
	}
	full := fixture(node, 4, true, "a")
	peers.Apply(full)
	if len(peers.Endpoints(time.Now())) != 1 {
		t.Fatal("complete inventory did not reconcile")
	}
	partial := fixture(node, 5, false)
	peers.Apply(partial)
	if len(peers.Endpoints(time.Now())) != 1 {
		t.Fatal("partial inventory inferred removal")
	}
	epoch := fixture(node, 1, true, "replacement")
	epoch.Epoch = 2
	peers.Apply(epoch)
	peers.Apply(full)
	if got := peers.Endpoints(time.Now()); len(got) != 1 || got[0].ID != "replacement" {
		t.Fatal("old epoch accepted")
	}
}

func TestPersistentEpochLockAndCorruption(t *testing.T) {
	dir := t.TempDir()
	node := uuid.NewString()
	e, err := AcquireEpoch(dir, node)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireEpoch(dir, node); err == nil {
		t.Fatal("duplicate publisher accepted")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireEpoch(dir, node)
	if err != nil {
		t.Fatal(err)
	}
	if next.Number <= e.Number {
		t.Fatal("epoch did not advance")
	}
	_ = next.Close()
	if err := os.WriteFile(filepath.Join(dir, node+".epoch"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireEpoch(dir, node); err == nil {
		t.Fatal("corrupt epoch reset")
	}
}

func TestMailboxKeepsCompleteBeforeNewerPartial(t *testing.T) {
	q := NewMailbox()
	node := uuid.New()
	q.Put(fixture(node, 1, true, "a"))
	q.Put(fixture(node, 3, false, "a", "b"))
	q.Put(fixture(node, 2, false, "a"))
	s, ok := q.Next()
	if !ok || !s.Complete || s.Revision != 1 {
		t.Fatal("complete inventory lost")
	}
	q.Done(s)
	s, ok = q.Next()
	if !ok || s.Revision != 3 {
		t.Fatal("latest revision lost")
	}
	q.Done(s)
	if _, ok := q.Next(); ok {
		t.Fatal("queue not drained")
	}
}

func TestRemovalBeforeFirstSnapshot(t *testing.T) {
	node := uuid.New()
	p := NewPeers(uuid.New(), time.Minute)
	removed := fixture(node, 3, false)
	removed.Removed = []string{"unknown"}
	p.Apply(removed)
	p.Apply(fixture(node, 2, true, "unknown"))
	if len(p.Endpoints(time.Now())) != 0 {
		t.Fatal("unseen removal resurrected by delayed snapshot")
	}
}

func TestObserveDoesNotNeedDatabaseOrBroker(t *testing.T) {
	node := uuid.New()
	a := NewAgent(node, "node-one", 1, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	done := make(chan struct{})
	go func() {
		a.Observe(runtime.Observation{Containers: []runtime.Container{{ID: "a", State: "running"}}, Complete: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observation blocked")
	}
	if len(a.pending()) != 1 {
		t.Fatal("publication not retained")
	}
	if _, err := OpenStore(context.Background(), "postgres://[::1]/fabric?sslmode=disable"); err == nil {
		t.Fatal("insecure DB config accepted")
	}
}
