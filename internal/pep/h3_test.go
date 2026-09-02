package pep

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/coded"
	"github.com/4fuu/niulang/internal/protocol"
	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
)

type oversizedH3DatagramStream struct{ limit int64 }

func (s oversizedH3DatagramStream) SendDatagram([]byte) error {
	return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: s.limit}
}

func (oversizedH3DatagramStream) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHTTPDatagramCarrierLearnsRequestStreamPayloadLimit(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	carrier := &h3DatagramCarrier{
		stream: oversizedH3DatagramStream{limit: 1100}, ctx: ctx, stop: stop,
	}
	carrier.limit.Store(1144)
	err := carrier.Send(make([]byte, 1144))
	if !errors.Is(err, coded.ErrDatagramTooLarge) {
		t.Fatalf("oversize error = %v, want coded.ErrDatagramTooLarge", err)
	}
	if got := carrier.MaxDatagramBytes(); got != 1092 {
		t.Fatalf("learned HTTP Datagram payload limit = %d, want 1092", got)
	}
	if err := carrier.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestQUICDataPlaneUsesRealHTTP3(t *testing.T) {
	serverCredentials, clientCredentials := testCertificate(t)
	packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Credentials: serverCredentials,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServePacketConn(ctx, packet) }()

	conn, clientPacket, err := dialQUICConnection(ctx, packet.LocalAddr().String(), clientCredentials, 5*time.Second, "", nil, nil, flowWindows{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer clientPacket.Close()
	defer conn.CloseWithError(h3NoErrorCode, "test complete")
	if got := conn.ConnectionState().TLS.NegotiatedProtocol; got != "h3" {
		cancel()
		t.Fatalf("negotiated ALPN = %q, want h3", got)
	}

	h3 := newH3ClientConn(conn)
	select {
	case <-h3.ReceivedSettings():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for HTTP/3 SETTINGS")
	}
	settings := h3.Settings()
	if settings == nil || !settings.EnableExtendedConnect || !settings.EnableDatagrams {
		cancel()
		t.Fatalf("HTTP/3 settings = %+v, want Extended CONNECT and HTTP Datagrams", settings)
	}
	stream, err := openH3Lane(ctx, h3, packet.LocalAddr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := stream.SendDatagram([]byte("h3-datagram")); err != nil {
		cancel()
		t.Fatalf("send RFC 9297 HTTP Datagram: %v", err)
	}
	if err := stream.Close(); err != nil {
		cancel()
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP/3 server did not shut down")
	}
}

func TestHTTP3LaneRequiresPeerDatagramSetting(t *testing.T) {
	serverCredentials, clientCredentials := testCertificate(t)
	packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Credentials: serverCredentials,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServePacketConn(ctx, packet) }()

	conn, clientPacket, err := dialQUICConnection(ctx, packet.LocalAddr().String(), clientCredentials, 5*time.Second, "", nil, nil, flowWindows{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer clientPacket.Close()
	defer conn.CloseWithError(h3NoErrorCode, "test complete")
	h3 := (&http3.Transport{}).NewClientConn(conn)
	select {
	case <-h3.ReceivedSettings():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for HTTP/3 SETTINGS")
	}
	stream, err := h3.OpenRequestStream(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request := &http.Request{
		Method: http.MethodConnect,
		URL: &url.URL{
			Scheme: "https",
			Host:   packet.LocalAddr().String(),
			Path:   protocol.H3TunnelPath,
		},
		Host: packet.LocalAddr().String(), Header: make(http.Header), Proto: protocol.H3TunnelProtocol,
	}
	if err := stream.SendRequestHeader(request); err != nil {
		cancel()
		t.Fatal(err)
	}
	response, err := stream.ReadResponse()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		cancel()
		t.Fatalf("CONNECT without peer HTTP Datagram setting returned %d, want 400", response.StatusCode)
	}

	cancel()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP/3 server did not shut down")
	}
}

func TestHTTP3ShutdownDrainsExistingLane(t *testing.T) {
	destination, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	go echoDestination(destination)

	serverCredentials, clientCredentials := testCertificate(t)
	packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Credentials: serverCredentials,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServePacketConn(serverCtx, packet) }()

	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		stopServer()
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: packet.LocalAddr().String(),
		Credentials: clientCredentials, EnableQUICPool: true, Logger: logger,
	})
	if err != nil {
		stopServer()
		t.Fatal(err)
	}
	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	go func() { _ = client.ServeListener(clientCtx, clientListener) }()

	flow := socksDial(t, clientListener.Addr().String(), destination, 10*time.Second)
	defer flow.Close()
	exchange := func(value byte) {
		t.Helper()
		if _, err := flow.Write([]byte{value}); err != nil {
			t.Fatal(err)
		}
		var reply [1]byte
		if _, err := io.ReadFull(flow, reply[:]); err != nil {
			t.Fatal(err)
		}
		if reply[0] != value {
			t.Fatalf("echo = %d, want %d", reply[0], value)
		}
	}
	exchange(1)
	stopServer()
	time.Sleep(50 * time.Millisecond)
	exchange(2)
	_ = flow.Close()

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP/3 server did not finish after its drained lane closed")
	}
}
