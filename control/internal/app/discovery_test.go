package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/discovery"
	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/miekg/dns"
)

// Exercise observation -> local snapshot -> real DNS sockets. Docker SDK
// translation is tested separately with an HTTP Docker simulator.
func TestObservationDrivesLocalDNS(t *testing.T) {
	cache, err := discovery.NewCache("fabric.internal", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	var phase atomic.Int32
	current := func() []runtime.Container {
		p := phase.Load()
		if p == 4 {
			return nil
		}
		address := "fd00::1"
		if p == 1 {
			address = "fd00::2"
		}
		if p == 3 {
			address = "fd00::3"
		}
		health := "healthy"
		if p == 2 {
			health = "unhealthy"
		}
		return []runtime.Container{{ID: "a", State: "running", Health: health, Labels: map[string]string{"fabric.discovery.service": "api", "fabric.discovery.network": "mesh"}, Networks: map[string][]string{"mesh": {address}}}}
	}
	events := make(chan runtime.Event, 1)
	a := testApp(fakeRuntime{
		list: func(context.Context) ([]runtime.Container, error) { return current(), nil },
		inspect: func(context.Context, string) (runtime.Container, error) {
			containers := current()
			if len(containers) == 0 {
				return runtime.Container{}, runtime.ErrNotFound
			}
			return containers[0], nil
		},
		watch: func(ctx context.Context, _ time.Time, emit func(runtime.Event) error) error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case e := <-events:
					if err := emit(e); err != nil {
						return err
					}
				}
			}
		},
	})
	a.RefreshInterval = 20 * time.Millisecond
	a.OnSnapshot = func(containers []runtime.Container) {
		endpoints, invalid := discovery.FromContainers("node-1", containers)
		if len(invalid) != 0 {
			t.Error("invalid fixture")
		}
		if err := cache.Replace(endpoints); err != nil {
			t.Error(err)
		}
	}
	ready := make(chan string, 1)
	server := &discovery.DNS{Cache: cache, TTL: 5, Address: "127.0.0.1:0", OnListening: func(address string) { ready <- address }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { done <- server.Run(ctx) }()
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for range 2 {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("services failed to join")
			}
		}
	})
	var address string
	select {
	case address = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("DNS did not start")
	}
	check := func(want string) {
		t.Helper()
		await(t, func() bool {
			m := new(dns.Msg)
			m.SetQuestion("api.service.fabric.internal.", dns.TypeAAAA)
			r, _, err := (&dns.Client{Timeout: time.Second}).Exchange(m, address)
			if err != nil {
				return false
			}
			if want == "" {
				return r.Rcode == dns.RcodeNameError
			}
			return r.Rcode == dns.RcodeSuccess && len(r.Answer) == 1 && r.Answer[0].(*dns.AAAA).AAAA.String() == want
		})
	}
	check("fd00::1")
	phase.Store(1)
	events <- runtime.Event{ContainerID: "a", Action: "connect"}
	check("fd00::2")
	phase.Store(2)
	events <- runtime.Event{ContainerID: "a", Action: "health_status: unhealthy"}
	check("")
	// Missed events are repaired by the observer's periodic full snapshot.
	phase.Store(3)
	check("fd00::3")
	phase.Store(4)
	events <- runtime.Event{ContainerID: "a", Action: "destroy"}
	check("")
}
