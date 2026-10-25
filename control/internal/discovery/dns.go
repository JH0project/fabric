package discovery

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

type DNS struct {
	Cache   *Cache
	TTL     uint32
	Address string
	// OnListening is called once after both listeners have started.
	OnListening func(address string)
	telemetry   *dnsTelemetry
}

func (s *DNS) ServeDNS(w dns.ResponseWriter, request *dns.Msg) {
	r := new(dns.Msg)
	r.SetReply(request)
	finish := s.telemetry.start(request)
	defer func() { finish(r.Rcode) }()
	if request.Opcode != dns.OpcodeQuery {
		r.Rcode = dns.RcodeNotImplemented
	} else if len(request.Question) != 1 {
		r.Rcode = dns.RcodeFormatError
	} else {
		q := request.Question[0]
		name := strings.ToLower(q.Name)
		switch {
		case q.Qclass != dns.ClassINET || !dns.IsSubDomain(s.Cache.Zone, name):
			r.Rcode = dns.RcodeRefused
		default:
			endpoints, ready, exists := s.Cache.lookup(name)
			r.Authoritative = true
			if !ready {
				r.Rcode = dns.RcodeServerFailure
				break
			}
			if name == s.Cache.Zone {
				if q.Qtype == dns.TypeSOA {
					r.Answer = append(r.Answer, s.soa())
				}
			} else if !exists {
				r.Rcode = dns.RcodeNameError
			} else {
				seen := map[string]bool{}
				ttl := s.TTL
				for _, e := range endpoints {
					if q.Qtype == dns.TypeSRV && strings.HasPrefix(name, "_") {
						rr := &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: ttl}, Priority: 10, Weight: 1, Port: e.Port, Target: e.target(s.Cache.Zone)}
						if !seen[rr.String()] {
							r.Answer = append(r.Answer, rr)
							seen[rr.String()] = true
						}
						for _, addr := range e.Addresses {
							if rr := addressRR(e.target(s.Cache.Zone), addr, ttl, 0); rr != nil {
								r.Extra = append(r.Extra, rr)
							}
						}
					} else if !strings.HasPrefix(name, "_") {
						for _, addr := range e.Addresses {
							if rr := addressRR(q.Name, addr, ttl, q.Qtype); rr != nil && !seen[rr.String()] {
								r.Answer = append(r.Answer, rr)
								seen[rr.String()] = true
							}
						}
					}
				}
			}
			if len(r.Answer) == 0 && r.Rcode != dns.RcodeServerFailure {
				r.Ns = append(r.Ns, s.soa())
			}
		}
	}
	// Respect classic UDP limits/EDNS; clients retry truncated answers over TCP.
	size := uint16(dns.MinMsgSize)
	if opt := request.IsEdns0(); opt != nil {
		if opt.Version() != 0 {
			r.SetEdns0(dns.MinMsgSize, false)
			r.Rcode = dns.RcodeBadVers
			_ = w.WriteMsg(r)
			return
		}
		size = opt.UDPSize()
		if size < dns.MinMsgSize {
			size = dns.MinMsgSize
		}
		if size > 1232 {
			size = 1232
		}
		r.SetEdns0(size, false)
	}
	if _, tcp := w.RemoteAddr().(*net.TCPAddr); !tcp {
		r.Truncate(int(size))
	} else {
		r.Truncate(dns.MaxMsgSize)
	}
	_ = w.WriteMsg(r)
}

func addressRR(name, addr string, ttl uint32, qtype uint16) dns.RR {
	ip := net.ParseIP(addr)
	if ip == nil {
		return nil
	}
	if ip.To4() != nil && (qtype == 0 || qtype == dns.TypeA) {
		return &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: ip.To4()}
	}
	if ip.To4() == nil && (qtype == 0 || qtype == dns.TypeAAAA) {
		return &dns.AAAA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}, AAAA: ip}
	}
	return nil
}

func (s *DNS) soa() *dns.SOA {
	return &dns.SOA{Hdr: dns.RR_Header{Name: s.Cache.Zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: s.TTL}, Ns: "ns." + s.Cache.Zone, Mbox: "hostmaster." + s.Cache.Zone, Serial: 1, Refresh: 30, Retry: 5, Expire: 60, Minttl: s.TTL}
}

// Run owns both listeners. A bind/serve failure stops the module.
func (s *DNS) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	if s.Cache == nil || s.TTL == 0 || s.TTL > 60 {
		return errors.New("invalid DNS dependencies or TTL")
	}
	var err error
	s.telemetry, err = newDNSTelemetry()
	if err != nil {
		return err
	}
	tcp, err := net.Listen("tcp", s.Address)
	if err != nil {
		return err
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		return err
	}
	defer udp.Close()
	servers := []*dns.Server{
		{Listener: tcp, Handler: s, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second},
		{PacketConn: udp, Handler: s, UDPSize: 1232, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second},
	}
	done := make(chan error, 2)
	ready := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, server := range servers {
		go func() { done <- serve(ctx, server, ready) }()
	}
	started, completed := 0, 0
	var result error
	for completed < len(servers) {
		select {
		case <-ready:
			started++
			if started == len(servers) && s.OnListening != nil && ctx.Err() == nil {
				s.OnListening(tcp.Addr().String())
			}
		case err := <-done:
			result = errors.Join(result, err)
			completed++
			cancel()
		}
	}
	return result
}

func serve(ctx context.Context, server *dns.Server, ready chan<- struct{}) error {
	started := make(chan struct{})
	done := make(chan error, 1)
	server.NotifyStartedFunc = func() { close(started) }
	go func() { done <- server.ActivateAndServe() }()
	// The library rejects shutdown before the startup callback. Each listener
	// owns its startup/result pair so an early error cannot leak its sibling.
	select {
	case <-started:
	case err := <-done:
		return errors.Join(errors.New("DNS startup failed"), err)
	}
	ready <- struct{}{}
	select {
	case err := <-done:
		return errors.Join(errors.New("DNS listener stopped unexpectedly"), err)
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := server.ShutdownContext(stopCtx)
		return errors.Join(err, <-done)
	}
}
