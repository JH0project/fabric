package discovery

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/miekg/dns"
)

func testCache(t *testing.T) *Cache {
	t.Helper()
	c, err := NewCache("fabric.internal", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func startDNS(t *testing.T, cache *Cache) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	s := &DNS{Cache: cache, TTL: 5, Address: "127.0.0.1:0", OnListening: func(address string) { ready <- address }}
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("DNS shutdown blocked")
		}
	})
	select {
	case addr := <-ready:
		return addr
	case <-time.After(5 * time.Second):
		t.Fatal("DNS startup blocked")
		return ""
	}
}

func query(t *testing.T, address, transport, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	r, _, err := (&dns.Client{Net: transport, Timeout: time.Second}).Exchange(m, address)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testEndpoint(id string) Endpoint {
	return Endpoint{Node: "node-1", ID: id, Service: "api", Addresses: []string{"10.42.0.2", "fd00::2"}, Port: 8080, PortName: "http", Protocol: "tcp"}
}

func TestDNSLifecycleAndAnswers(t *testing.T) {
	c := testCache(t)
	addr := startDNS(t, c)
	name := "api.service.fabric.internal."
	if r := query(t, addr, "udp", name, dns.TypeA); r.Rcode != dns.RcodeServerFailure {
		t.Fatalf("bootstrap: %v", r)
	}
	e := testEndpoint("a")
	if err := c.Replace([]Endpoint{e}); err != nil {
		t.Fatal(err)
	}
	// Snapshot ownership: the publisher cannot mutate the live cache.
	e.Addresses[0] = "10.42.0.99"
	for _, transport := range []string{"udp", "tcp"} {
		for _, name := range []string{name, "API.SERVICE.FABRIC.INTERNAL.", "api.service.node-1.node.fabric.internal.", "api.service.local.node.fabric.internal."} {
			r := query(t, addr, transport, name, dns.TypeA)
			if r.Rcode != dns.RcodeSuccess || !r.Authoritative || r.RecursionAvailable || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "10.42.0.2" {
				t.Fatalf("A: %v", r)
			}
			if r.Answer[0].Header().Ttl != 5 {
				t.Fatal("TTL")
			}
		}
		r := query(t, addr, transport, name, dns.TypeAAAA)
		if len(r.Answer) != 1 || r.Answer[0].(*dns.AAAA).AAAA.String() != "fd00::2" {
			t.Fatalf("AAAA: %v", r)
		}
		r = query(t, addr, transport, "_http._tcp."+name, dns.TypeSRV)
		if len(r.Answer) != 1 || len(r.Extra) != 2 {
			t.Fatalf("SRV/glue: %v", r)
		}
		srv := r.Answer[0].(*dns.SRV)
		if srv.Port != 8080 || !strings.HasSuffix(srv.Target, ".endpoint.fabric.internal.") {
			t.Fatalf("SRV target: %v", srv)
		}
		if got := query(t, addr, transport, srv.Target, dns.TypeA); len(got.Answer) != 1 {
			t.Fatalf("unresolvable SRV target: %v", got)
		}
		for _, missing := range []string{"api.service.node-2.node.fabric.internal.", "unknown.service.fabric.internal."} {
			r = query(t, addr, transport, missing, dns.TypeA)
			if r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 {
				t.Fatalf("NXDOMAIN: %v", r)
			}
		}
		for _, existing := range []string{name, "service.fabric.internal."} {
			r = query(t, addr, transport, existing, dns.TypeTXT)
			if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
				t.Fatalf("NODATA/empty nonterminal: %v", r)
			}
		}
		if r = query(t, addr, transport, "example.com.", dns.TypeA); r.Rcode != dns.RcodeRefused || r.Authoritative {
			t.Fatalf("out of zone: %v", r)
		}
	}
	if err := c.Replace([]Endpoint{{}}); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if r := query(t, addr, "udp", name, dns.TypeA); len(r.Answer) != 1 {
		t.Fatal("invalid update lost last snapshot")
	}
	if err := c.Replace(nil); err != nil {
		t.Fatal(err)
	}
	if r := query(t, addr, "udp", name, dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("removal: %v", r)
	}
}

func TestTruncationAndConcurrentReplacement(t *testing.T) {
	c := testCache(t)
	var endpoints []Endpoint
	for i := 1; i <= 100; i++ {
		e := testEndpoint(fmt.Sprint(i))
		e.Addresses = []string{fmt.Sprintf("fd00::%x", i)}
		endpoints = append(endpoints, e)
	}
	if err := c.Replace(endpoints); err != nil {
		t.Fatal(err)
	}
	addr := startDNS(t, c)
	r := query(t, addr, "udp", "api.service.fabric.internal.", dns.TypeAAAA)
	// The client unpacks without preserving the sender's compression setting.
	r.Compress = true
	if !r.Truncated || r.Len() > 512 {
		t.Fatalf("UDP not truncated: %d, %v", r.Len(), r.Truncated)
	}
	r = query(t, addr, "tcp", "api.service.fabric.internal.", dns.TypeAAAA)
	if r.Truncated || len(r.Answer) != 100 {
		t.Fatalf("TCP: %d answers", len(r.Answer))
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			if err := c.Replace(endpoints); err != nil {
				t.Error(err)
			}
		}
	})
	for range 20 {
		if r = query(t, addr, "udp", "api.service.fabric.internal.", dns.TypeA); r.Rcode != dns.RcodeSuccess {
			t.Error(r)
		}
	}
	wg.Wait()
}

func TestUDPBindFailureClosesTCP(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	s := &DNS{Cache: testCache(t), TTL: 5, Address: udp.LocalAddr().String()}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("UDP bind failure ignored")
	}
	tcp, err := net.Listen("tcp", udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("TCP listener leaked: %v", err)
	}
	tcp.Close()
}

func TestFromContainersOptInAndHealth(t *testing.T) {
	base := runtime.Container{ID: "a", State: "running", Labels: map[string]string{"fabric.discovery.service": "api", "fabric.discovery.network": "mesh", "fabric.discovery.port": "8080", "fabric.discovery.port-name": "http"}, Networks: map[string][]string{"mesh": {"fd00::2", "10.42.0.2"}, "bridge": {"172.17.0.2"}}}
	endpoints, invalid := FromContainers("node-1", []runtime.Container{base, {ID: "unlabelled", State: "running"}})
	if len(endpoints) != 1 || len(invalid) != 0 || len(endpoints[0].Addresses) != 2 || endpoints[0].Port != 8080 {
		t.Fatalf("selection: %+v, %v", endpoints, invalid)
	}
	for _, state := range []string{"exited", "paused", "restarting"} {
		c := base
		c.State = state
		if endpoints, _ := FromContainers("node-1", []runtime.Container{c}); len(endpoints) != 0 {
			t.Fatal("registered stopped container")
		}
	}
	for _, health := range []string{"unhealthy", "starting"} {
		c := base
		c.Health = health
		if endpoints, _ := FromContainers("node-1", []runtime.Container{c}); len(endpoints) != 0 {
			t.Fatal("registered unready container")
		}
	}
	for _, bad := range []struct{ key, value string }{
		{"fabric.discovery.service", "Bad_Name"}, {"fabric.discovery.network", "missing"}, {"fabric.discovery.port", "65536"}, {"fabric.discovery.port", "0"}, {"fabric.discovery.protocol", "sctp"}, {"fabric.discovery.addresses", "127.0.0.1"}, {"fabric.discovery.addresses", "::"}, {"fabric.discovery.addresses", "fe80::1%eth0"},
	} {
		c := base
		c.Labels = make(map[string]string)
		for k, v := range base.Labels {
			c.Labels[k] = v
		}
		c.Labels[bad.key] = bad.value
		if endpoints, invalid := FromContainers("node-1", []runtime.Container{c}); len(endpoints) != 0 || len(invalid) != 1 {
			t.Fatalf("accepted invalid label %s", bad.key)
		}
	}
	base.Labels["fabric.discovery.addresses"] = "fd00::5, 10.0.0.1,10.0.0.1"
	if endpoints, invalid := FromContainers("node-1", []runtime.Container{base}); len(invalid) != 0 || len(endpoints[0].Addresses) != 2 {
		t.Fatalf("explicit addresses: %v, %v", endpoints, invalid)
	}
}
