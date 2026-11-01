package inventory

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Mailbox retains the newest complete inventory separately from the newest
// update, so coalescing partial observations cannot discard reconciliation.
type Mailbox struct {
	mu       sync.Mutex
	pending  map[uuid.UUID]Snapshot
	complete map[uuid.UUID]Snapshot
	wake     chan struct{}
}

func NewMailbox() *Mailbox {
	return &Mailbox{pending: map[uuid.UUID]Snapshot{}, complete: map[uuid.UUID]Snapshot{}, wake: make(chan struct{}, 1)}
}
func (q *Mailbox) Put(s Snapshot) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) >= 256 {
		if _, ok := q.pending[s.NodeID]; !ok {
			return false
		}
	}
	if old, ok := q.pending[s.NodeID]; !ok || Newer(s, old) {
		q.pending[s.NodeID] = s
	}
	if s.Complete {
		if old, ok := q.complete[s.NodeID]; !ok || Newer(s, old) {
			q.complete[s.NodeID] = s
		}
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}
func (q *Mailbox) Next() (Snapshot, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, s := range q.pending {
		if full, ok := q.complete[id]; ok && !Newer(full, s) {
			return full, true
		}
		return s, true
	}
	return Snapshot{}, false
}
func (q *Mailbox) Done(s Snapshot) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if old, ok := q.complete[s.NodeID]; ok && old.Epoch == s.Epoch && old.Revision == s.Revision {
		delete(q.complete, s.NodeID)
	}
	if old, ok := q.pending[s.NodeID]; ok && old.Epoch == s.Epoch && old.Revision == s.Revision {
		delete(q.pending, s.NodeID)
	}
}

type Persistence struct {
	Bus     SnapshotSource
	Store   SnapshotStore
	Logger  *slog.Logger
	Pending *Mailbox
}

func (p *Persistence) Name() string { return "inventory-persistence" }

func (p *Persistence) Run(ctx context.Context) error {
	if p.Bus == nil || p.Store == nil || p.Logger == nil {
		return errors.New("invalid persistence dependencies")
	}
	q := p.Pending
	if q == nil {
		q = NewMailbox()
	}
	stop, err := p.Bus.Subscribe(func(s Snapshot) {
		if !q.Put(s) {
			p.Logger.Warn("inventory persistence mailbox full")
		}
	})
	if err != nil {
		return err
	}
	defer stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	request := time.NewTicker(15 * time.Second)
	defer request.Stop()
	ask := func() {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = p.Bus.RequestSnapshots(c)
	}
	ask()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-request.C:
			ask()
		case <-q.wake:
		case <-tick.C:
		}
		for count := 0; count < 256; count++ {
			s, ok := q.Next()
			if !ok {
				break
			}
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := p.Store.Apply(c, s)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					p.Logger.Warn("inventory persistence retry pending", "error", err)
				}
				break
			}
			q.Done(s)
		}
	}
}
