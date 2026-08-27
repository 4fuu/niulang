package pep

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/4fuu/niulang/internal/coded"
	wancongestion "github.com/4fuu/niulang/internal/congestion"
	"github.com/4fuu/niulang/internal/identity"
	"github.com/4fuu/niulang/internal/protocol"
	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
)

const httpDatagramOverhead = 56

const (
	h3NoErrorCode         = quic.ApplicationErrorCode(http3.ErrCodeNoError)
	h3ExcessiveLoadCode   = quic.ApplicationErrorCode(http3.ErrCodeExcessiveLoad)
	h3RequestCanceledCode = quic.StreamErrorCode(http3.ErrCodeRequestCanceled)
	h3DrainTimeout        = 2 * time.Second
)

func newH3ClientConn(conn *quic.Conn) *http3.ClientConn {
	return (&http3.Transport{EnableDatagrams: true}).NewClientConn(conn)
}

func openH3Lane(ctx context.Context, conn *http3.ClientConn, remote string) (*h3ClientStream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-conn.ReceivedSettings():
	}
	settings := conn.Settings()
	if settings == nil || !settings.EnableExtendedConnect {
		return nil, errors.New("gateway did not enable HTTP/3 Extended CONNECT")
	}
	if !settings.EnableDatagrams {
		return nil, errors.New("gateway did not enable HTTP Datagrams")
	}

	stream, err := conn.OpenRequestStream(ctx)
	if err != nil {
		return nil, err
	}
	cancel := func() {
		stream.CancelRead(h3RequestCanceledCode)
		stream.CancelWrite(h3RequestCanceledCode)
	}
	request := &http.Request{
		Method: http.MethodConnect,
		URL: &url.URL{
			Scheme: "https",
			Host:   remote,
			Path:   protocol.H3TunnelPath,
		},
		Host:   remote,
		Header: make(http.Header),
		Proto:  protocol.H3TunnelProtocol,
	}
	if err := stream.SendRequestHeader(request); err != nil {
		cancel()
		return nil, fmt.Errorf("send Niulang HTTP/3 CONNECT: %w", err)
	}
	return &h3ClientStream{RequestStream: stream}, nil
}

// h3ClientStream sends Niulang bytes optimistically after the CONNECT headers
// and validates the response on the first read. Waiting for the 200 before a
// write would add one round trip to every warm pooled flow even though the
// server has all authorization evidence in the connection's mutual TLS state.
type h3ClientStream struct {
	*http3.RequestStream
	responseOnce sync.Once
	responseErr  error
}

func (s *h3ClientStream) Read(payload []byte) (int, error) {
	if err := s.waitH3Response(); err != nil {
		return 0, err
	}
	return s.RequestStream.Read(payload)
}
func (s *h3ClientStream) waitH3Response() error {
	s.responseOnce.Do(func() {
		response, err := s.RequestStream.ReadResponse()
		if err != nil {
			s.responseErr = fmt.Errorf("read Niulang HTTP/3 CONNECT response: %w", err)
			return
		}
		if response.StatusCode != http.StatusOK {
			s.responseErr = fmt.Errorf("gateway refused Niulang HTTP/3 CONNECT with status %d", response.StatusCode)
		}
	})
	if s.responseErr != nil {
		return s.responseErr
	}
	return nil
}

type h3DatagramStream interface {
	SendDatagram([]byte) error
	ReceiveDatagram(context.Context) ([]byte, error)
}

// h3DatagramCarrier carries coded frames in RFC 9297 HTTP Datagrams scoped to
// one Extended CONNECT request stream. Its own receive context can be canceled
// without closing the HTTP/3 connection shared by other lanes.
type h3DatagramCarrier struct {
	stream h3DatagramStream
	ctx    context.Context
	stop   context.CancelFunc
	limit  atomic.Int64
}

func newH3DatagramCarrier(stream h3DatagramStream, conn *quic.Conn) *h3DatagramCarrier {
	ctx, stop := context.WithCancel(context.Background())
	carrier := &h3DatagramCarrier{stream: stream, ctx: ctx, stop: stop}
	limit := int64(conn.InitialPacketSize()) - httpDatagramOverhead
	if limit < 1 {
		limit = 1
	}
	carrier.limit.Store(limit)
	return carrier
}

func newH3CodedPath(stream h3DatagramStream, conn *quic.Conn, queueFrames int) *coded.Path {
	if support := conn.ConnectionState().SupportsDatagrams; !support.Local || !support.Remote {
		return nil
	}
	return newCodedPath(newH3DatagramCarrier(stream, conn), peerKey(conn), 0, queueFrames)
}

func (c *h3DatagramCarrier) MaxDatagramBytes() int { return int(c.limit.Load()) }

func (c *h3DatagramCarrier) Send(payload []byte) error {
	// A Datagram may overtake the CONNECT HEADERS and be discarded before the
	// peer registers this request stream. DATA protects that bounded race with
	// its reliable first-frame copy and FEC repairs; PACKET retains normal UDP
	// loss semantics. Waiting for status 200 here would instead add one full
	// round trip to every new coded lane and block all of its repair symbols.
	err := c.stream.SendDatagram(payload)
	if err == nil {
		return nil
	}
	var tooLarge *quic.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		// quic-go reports the connection-level DATAGRAM payload limit. RFC
		// 9297 additionally prefixes the request stream's quarter stream ID,
		// which can occupy up to eight bytes.
		limit := tooLarge.MaxDatagramPayloadSize - 8
		if limit < 1 {
			limit = 1
		}
		c.limit.Store(limit)
		return fmt.Errorf("%w: %d bytes offered, %d accepted", coded.ErrDatagramTooLarge, len(payload), limit)
	}
	return err
}

func (c *h3DatagramCarrier) Receive() ([]byte, error) {
	return c.stream.ReceiveDatagram(c.ctx)
}

func (c *h3DatagramCarrier) Close() error {
	c.stop()
	return nil
}

type h3ServerStateKey struct{}
type h3ShutdownContextKey struct{}

func h3ServerShuttingDown(ctx context.Context) bool {
	shutdown, _ := ctx.Value(h3ShutdownContextKey{}).(context.Context)
	return shutdown != nil && shutdown.Err() != nil
}

type h3ServerConnState struct {
	conn       *quic.Conn
	laneCtx    context.Context
	principal  identity.Principal
	controller wancongestion.TelemetryProvider
	ccfg       congestionConfig
	auth       *quicAuthState
	authOnce   sync.Once
	err        error
}

func (s *h3ServerConnState) authenticate(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.conn.HandshakeComplete():
	}
	s.authOnce.Do(func() {
		s.principal, s.err = identity.PrincipalFromTLS(s.conn.ConnectionState().TLS)
		if s.err == nil {
			s.controller = configureQUICController(s.conn, s.ccfg)
			s.auth = &quicAuthState{principal: s.principal}
		}
	})
	return s.err
}

func (s *Server) newH3Server(laneCtx context.Context, tlsConfig *tls.Config, qcfg *quic.Config) *http3.Server {
	server := &http3.Server{
		TLSConfig:       tlsConfig,
		QUICConfig:      qcfg,
		EnableDatagrams: true,
		MaxHeaderBytes:  8 << 10,
	}
	server.ConnContext = func(ctx context.Context, conn *quic.Conn) context.Context {
		state := &h3ServerConnState{conn: conn, laneCtx: laneCtx, ccfg: congestionConfig{
			kind: s.cfg.Congestion, brutalBytesPerSecond: s.cfg.BrutalBytesPerSec,
			adaptiveMinBytesPerSec: s.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: s.cfg.AdaptiveMaxBytesSec,
			wireCaps: s.wireCaps,
		}}
		return context.WithValue(ctx, h3ServerStateKey{}, state)
	}
	server.Handler = http.HandlerFunc(s.handleH3Lane)
	return server
}

func (s *Server) handleH3Lane(w http.ResponseWriter, request *http.Request) {
	state, ok := request.Context().Value(h3ServerStateKey{}).(*h3ServerConnState)
	if !ok || state == nil || state.authenticate(request.Context()) != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if request.Method != http.MethodConnect || request.Proto != protocol.H3TunnelProtocol ||
		request.URL.Path != protocol.H3TunnelPath || request.URL.RawQuery != "" {
		http.NotFound(w, request)
		return
	}
	settings, ok := w.(interface {
		ReceivedSettings() <-chan struct{}
		Settings() *http3.Settings
	})
	if !ok {
		http.Error(w, "HTTP/3 settings unavailable", http.StatusInternalServerError)
		return
	}
	select {
	case <-request.Context().Done():
		return
	case <-settings.ReceivedSettings():
	}
	peerSettings := settings.Settings()
	if peerSettings == nil || !peerSettings.EnableDatagrams {
		http.Error(w, "HTTP Datagrams required", http.StatusBadRequest)
		return
	}
	streamer, ok := w.(http3.HTTPStreamer)
	if !ok {
		http.Error(w, "HTTP/3 stream unavailable", http.StatusInternalServerError)
		return
	}

	ordinary := false
	select {
	case s.semaphore <- struct{}{}:
		ordinary = true
	default:
		select {
		case s.probeOverflow <- struct{}{}:
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			s.cfg.Logger.Warn("remote session limit reached")
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	stream := streamer.HTTPStream()
	lane := &quicStreamConn{
		stream: stream, conn: state.conn, controller: state.controller, metrics: s.metrics,
		closeConn: false, cancelReadCode: h3RequestCanceledCode, bulk: newH3CodedPath(stream, state.conn, 0),
	}
	if ordinary {
		defer func() { <-s.semaphore }()
		s.handleSession(state.laneCtx, lane, state.principal, state.auth)
		return
	}
	defer func() { <-s.probeOverflow }()
	s.handleOverflowPathProbe(lane, state.principal, state.auth)
}
