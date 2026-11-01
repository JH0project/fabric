package inventory

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/JH0project/fabric/control/internal/discovery"
	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/google/uuid"
)

type Agent struct {
	NodeID       uuid.UUID
	NodeName     string
	Epoch        uint64
	Bus          SnapshotTransport
	Logger       *slog.Logger
	Peers        *Peers
	DNS          *discovery.Cache
	mu           sync.Mutex
	projectionMu sync.Mutex
	latest       *Snapshot
	complete     *Snapshot
	wake         chan struct{}
	revision     uint64
}

func NewAgent(node uuid.UUID, name string, epoch uint64, bus SnapshotTransport, logger *slog.Logger, cache *discovery.Cache) *Agent {
	return &Agent{NodeID: node, NodeName: name, Epoch: epoch, Bus: bus, Logger: logger, DNS: cache, Peers: NewPeers(node, 60*time.Second), wake: make(chan struct{}, 1)}
}

// Observe does local work only, never waits for a broker or SQL connection.
func (a *Agent) Observe(o runtime.Observation) {
	a.mu.Lock()
	a.revision++
	s := Project(a.NodeID, a.NodeName, a.Epoch, a.revision, o, time.Now())
	a.latest = &s
	if s.Complete {
		a.complete = &s
	}
	a.mu.Unlock()
	a.Peers.Apply(s)
	a.rebuild()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}
func (a *Agent) rebuild() {
	a.projectionMu.Lock()
	defer a.projectionMu.Unlock()
	if a.DNS != nil {
		if err := a.DNS.Replace(a.Peers.Endpoints(time.Now())); err != nil {
			a.Logger.Warn("inventory DNS projection retained", "error", err)
		}
	}
}
func (a *Agent) pending() []Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	var snapshots []Snapshot
	if a.complete != nil {
		snapshots = append(snapshots, *a.complete)
	}
	if a.latest != nil && (a.complete == nil || Newer(*a.latest, *a.complete)) {
		snapshots = append(snapshots, *a.latest)
	}
	return snapshots
}
func (a *Agent) Name() string { return "inventory-sync" }

func (a *Agent) Run(ctx context.Context) error {
	if a.Bus == nil || a.Logger == nil || a.NodeID == uuid.Nil || !ValidName(a.NodeName) || a.Epoch == 0 {
		return errors.New("invalid inventory agent dependencies")
	}
	unsub, err := a.Bus.Subscribe(func(s Snapshot) {
		if s.NodeID != a.NodeID {
			a.Peers.Apply(s)
			a.rebuild()
		}
	})
	if err != nil {
		return err
	}
	defer unsub()
	stopRequests, err := a.Bus.ListenRequests()
	if err != nil {
		return err
	}
	defer stopRequests()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	request := time.NewTicker(15 * time.Second)
	defer request.Stop()
	var sentComplete, sentLatest uint64
	for {
		force := false
		select {
		case <-ctx.Done():
			return nil
		case <-request.C:
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = a.Bus.RequestSnapshots(c)
			cancel()
		case <-tick.C:
			a.rebuild()
		case <-a.wake:
		case <-a.Bus.Requests():
			force = true
		}
		for _, s := range a.pending() {
			if !force && (s.Complete && s.Revision <= sentComplete || !s.Complete && s.Revision <= sentLatest) {
				continue
			}
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := a.Bus.Publish(c, s)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					a.Logger.Debug("inventory publication pending", "error", err)
				}
				break
			}
			if s.Complete {
				sentComplete = s.Revision
			} else {
				sentLatest = s.Revision
			}
		}
	}
}
