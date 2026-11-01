package inventory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

type Publisher interface {
	Publish(context.Context, Snapshot) error
}

// SnapshotSource is a domain-level subscription boundary. Components depend on
// this contract, not NATS messages or connection objects.
type SnapshotSource interface {
	Subscribe(func(Snapshot)) (func(), error)
	RequestSnapshots(context.Context) error
}

type SnapshotTransport interface {
	SnapshotSource
	Publisher
	ListenRequests() (func(), error)
	Requests() <-chan struct{}
}
type SnapshotStore interface {
	Apply(context.Context, Snapshot) error
}

type TLSFiles struct{ CA, Certificate, Key string }

func (f TLSFiles) Load() (*tls.Config, error) {
	if f.CA == "" || (f.Certificate == "") != (f.Key == "") {
		return nil, errors.New("CA is required; optional client certificate/key must be supplied together")
	}
	pem, err := os.ReadFile(f.CA)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid CA certificate")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if f.Certificate != "" {
		cert, err := tls.LoadX509KeyPair(f.Certificate, f.Key)
		if err != nil {
			return nil, err
		}
		config.Certificates = []tls.Certificate{cert}
	}
	return config, nil
}

// DialIPv6 never falls back to IPv4, including broker-discovered peer addresses.
func DialIPv6(ctx context.Context, _, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp6", address)
}

type ipv6Dialer struct{}

func (ipv6Dialer) Dial(network, address string) (net.Conn, error) {
	return DialIPv6(context.Background(), network, address)
}

type Bus struct {
	conn      *nats.Conn
	cluster   string
	requests  chan struct{}
	operation *operation
}

type NATSAuth struct{ Username, Password string }

func NewBus(urls, cluster string, files TLSFiles) (*Bus, error) {
	return NewBusWithAuth(urls, cluster, files, NATSAuth{})
}

func NewBusWithAuth(urls, cluster string, files TLSFiles, auth NATSAuth) (*Bus, error) {
	if !ValidName(cluster) {
		return nil, errors.New("invalid inventory cluster name")
	}
	for _, raw := range strings.Split(urls, ",") {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme != "tls" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("inventory NATS URLs must use TLS without embedded credentials")
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && !NativeIPv6(u.Hostname()) {
			return nil, errors.New("inventory NATS endpoints must use IPv6")
		}
	}
	tlsConfig, err := files.Load()
	if err != nil {
		return nil, err
	}
	if (auth.Username == "") != (auth.Password == "") {
		return nil, errors.New("NATS username and password must be supplied together")
	}
	if auth.Username == "" && len(tlsConfig.Certificates) == 0 {
		return nil, errors.New("NATS requires username/password or client certificate authentication")
	}
	op, err := newOperation("publish")
	if err != nil {
		return nil, err
	}
	b := &Bus{cluster: cluster, requests: make(chan struct{}, 1), operation: op}
	ready := func(c *nats.Conn) {
		_ = c.Publish(b.requestSubject(), nil)
		select {
		case b.requests <- struct{}{}:
		default:
		}
	}
	options := []nats.Option{nats.Name("fabric-inventory-" + cluster), nats.Secure(tlsConfig), nats.SetCustomDialer(ipv6Dialer{}), nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second), nats.ReconnectBufSize(0), nats.Timeout(3 * time.Second), nats.ConnectHandler(ready), nats.ReconnectHandler(ready)}
	if auth.Username != "" {
		options = append(options, nats.UserInfo(auth.Username, auth.Password))
	}
	b.conn, err = nats.Connect(urls, options...)
	if err != nil {
		return nil, errors.New("create inventory NATS connection failed")
	}
	return b, nil
}
func (b *Bus) snapshotSubject() string { return "fabric.inventory.v1." + b.cluster + ".snapshot." }
func (b *Bus) requestSubject() string  { return "fabric.inventory.v1." + b.cluster + ".request" }
func (b *Bus) Publish(ctx context.Context, s Snapshot) (err error) {
	ctx, finish := b.operation.start(ctx)
	defer func() { finish(err) }()
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(data) > MaxMessageBytes {
		return errors.New("inventory snapshot exceeds message size limit")
	}
	if !b.conn.IsConnected() {
		return errors.New("inventory NATS unavailable")
	}
	if err = b.conn.Publish(b.snapshotSubject()+s.NodeID.String(), data); err != nil {
		return err
	}
	// Flush is transport acceptance, never a durable JetStream acknowledgment.
	return b.conn.FlushWithContext(ctx)
}
func (b *Bus) Subscribe(handle func(Snapshot)) (func(), error) {
	sub, err := b.conn.Subscribe(b.snapshotSubject()+"*", func(m *nats.Msg) {
		if len(m.Data) > MaxMessageBytes {
			return
		}
		var s Snapshot
		if json.Unmarshal(m.Data, &s) != nil || s.Validate() != nil || m.Subject != b.snapshotSubject()+s.NodeID.String() || s.ObservedAt.After(time.Now().Add(30*time.Second)) {
			return
		}
		handle(s)
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
func (b *Bus) ListenRequests() (func(), error) {
	sub, err := b.conn.Subscribe(b.requestSubject(), func(m *nats.Msg) {
		if len(m.Data) == 0 {
			select {
			case b.requests <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
func (b *Bus) RequestSnapshots(ctx context.Context) error {
	if !b.conn.IsConnected() {
		return errors.New("inventory NATS unavailable")
	}
	if err := b.conn.FlushWithContext(ctx); err != nil {
		return err
	}
	if err := b.conn.Publish(b.requestSubject(), nil); err != nil {
		return err
	}
	return b.conn.FlushWithContext(ctx)
}
func (b *Bus) Close() { b.conn.Close() }

func (b *Bus) Requests() <-chan struct{} { return b.requests }
