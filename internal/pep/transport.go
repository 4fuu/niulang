package pep

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type TransportKind string

const (
	TransportTCP  TransportKind = "tcp"
	TransportQUIC TransportKind = "quic"
	TransportAuto TransportKind = "auto"
)

const defaultALPN = "wanopt/1"

type udpHealth struct {
	mu        sync.Mutex
	failures  int
	threshold int
	cooldown  time.Duration
	blockedTo time.Time
}

func newUDPHealth(threshold int, cooldown time.Duration) *udpHealth {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &udpHealth{threshold: threshold, cooldown: cooldown}
}

func (h *udpHealth) allow(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !now.Before(h.blockedTo)
}

func (h *udpHealth) success() {
	h.mu.Lock()
	h.failures = 0
	h.blockedTo = time.Time{}
	h.mu.Unlock()
}

func (h *udpHealth) failure(now time.Time) {
	h.mu.Lock()
	h.failures++
	if h.failures >= h.threshold {
		h.blockedTo = now.Add(h.cooldown)
		h.failures = 0
	}
	h.mu.Unlock()
}

type streamConn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

type quicStreamConn struct {
	stream *quic.Stream
	conn   *quic.Conn
	once   sync.Once
}

func (c *quicStreamConn) Read(p []byte) (int, error)  { return c.stream.Read(p) }
func (c *quicStreamConn) Write(p []byte) (int, error) { return c.stream.Write(p) }
func (c *quicStreamConn) SetDeadline(t time.Time) error {
	return c.stream.SetDeadline(t)
}
func (c *quicStreamConn) Close() error {
	var err error
	c.once.Do(func() {
		// Closing the connection as well as the stream ensures that a lane
		// cannot remain alive after a flow has been terminated. A future
		// session manager may share a QUIC connection and will use a different
		// wrapper with stream-only lifetime.
		_ = c.stream.Close()
		err = c.conn.CloseWithError(0, "wanopt lane closed")
	})
	return err
}

func tlsClientConfig(serverName string, roots *x509.CertPool) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, NextProtos: []string{defaultALPN}}
	if roots != nil {
		cfg.RootCAs = roots
	}
	return cfg
}

func dialTCP(ctx context.Context, remote, serverName string, roots *x509.CertPool, dialTimeout time.Duration) (streamConn, error) {
	conn, err := (&tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second},
		Config:    tlsClientConfig(serverName, roots),
	}).DialContext(ctx, "tcp", remote)
	if err != nil {
		return nil, err
	}
	tlsConn := conn.(*tls.Conn)
	if tlsConn.ConnectionState().NegotiatedProtocol != defaultALPN {
		_ = tlsConn.Close()
		return nil, errors.New("remote did not negotiate wanopt ALPN")
	}
	return tlsConn, nil
}

func quicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:           10 * time.Second,
		MaxIdleTimeout:                 2 * time.Minute,
		KeepAlivePeriod:                20 * time.Second,
		InitialStreamReceiveWindow:     512 * 1024,
		MaxStreamReceiveWindow:         8 * 1024 * 1024,
		InitialConnectionReceiveWindow: 1 * 1024 * 1024,
		MaxConnectionReceiveWindow:     16 * 1024 * 1024,
		MaxIncomingStreams:             1,
		MaxIncomingUniStreams:          0,
	}
}

func dialQUIC(ctx context.Context, remote, serverName string, roots *x509.CertPool, dialTimeout time.Duration) (streamConn, error) {
	dialCtx := ctx
	var cancel context.CancelFunc
	if dialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	tlsCfg := tlsClientConfig(serverName, roots)
	conn, err := quic.DialAddr(dialCtx, remote, tlsCfg, quicConfig())
	if err != nil {
		return nil, err
	}
	stream, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "unable to open wanopt stream")
		return nil, err
	}
	return &quicStreamConn{stream: stream, conn: conn}, nil
}

func quicServerTLSConfig(certificate tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{defaultALPN}}
}

func quicServerConfig() *quic.Config {
	cfg := quicConfig()
	cfg.MaxIncomingStreams = 1
	return cfg
}

func acceptQUICStream(ctx context.Context, conn *quic.Conn) (streamConn, error) {
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStreamConn{stream: stream, conn: conn}, nil
}

func transportError(kind TransportKind, err error) error {
	return fmt.Errorf("%s lane: %w", kind, err)
}
