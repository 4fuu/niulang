package pep

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/socks5"
)

// TestUDPAssociationRescuesToTCP keeps the SOCKS UDP endpoint fixed while the
// established QUIC listener is deliberately withdrawn. The second datagram
// must traverse a newly authenticated TCP association; this exercises the
// user-visible failure behavior rather than only the new-flow transport race.
func TestUDPAssociationRescuesToTCP(t *testing.T) {
	destination, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, readErr := destination.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			_, _ = destination.WriteToUDP(buf[:n], addr)
		}
	}()

	tlsCert, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
	quicPacketConn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpPort)))
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := tcpListener.Addr().String()
	server, err := NewServer(ServerConfig{
		ListenAddr: serverAddr, Credentials: tlsCert,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, EnableQUIC: true,
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientListener.Close()
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: serverAddr, Credentials: roots, Transport: TransportAuto, FallbackDelay: 5 * time.Second,
		UDPFailureThreshold: 1, UDPCooldown: time.Minute, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	defer serviceCancel()
	quicCtx, quicCancel := context.WithCancel(serviceCtx)
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- server.ServeListener(serviceCtx, tcpListener) }()
	go func() { errorsCh <- server.ServePacketConn(quicCtx, quicPacketConn) }()
	go func() { errorsCh <- client.ServeListener(serviceCtx, clientListener) }()

	control, err := net.DialTimeout("tcp", clientListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	bound := openTestUDPAssociation(t, control)
	udpClient, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udpClient.Close()
	destinationAddr := destination.LocalAddr().(*net.UDPAddr)

	send := func(payload string) {
		t.Helper()
		request := []byte{0, 0, 0, 1}
		request = append(request, destinationAddr.IP.To4()...)
		var portBytes [2]byte
		binary.BigEndian.PutUint16(portBytes[:], uint16(destinationAddr.Port))
		request = append(request, portBytes[:]...)
		request = append(request, payload...)
		if _, err := udpClient.WriteToUDP(request, bound); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2048)
		_ = udpClient.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := udpClient.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		got, err := socks5.ReadUDPDatagram(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Payload) != payload {
			t.Fatalf("UDP rescue echo %q, want %q", got.Payload, payload)
		}
	}

	send("before-rescue")
	quicCancel()
	deadline := time.Now().Add(5 * time.Second)
	for client.Metrics().Snapshot().UDPAssociationReconnects == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := client.Metrics().Snapshot(); got.UDPAssociationReconnects == 0 {
		t.Fatalf("QUIC lane failure did not trigger UDP rescue: %+v", got)
	}
	send("after-rescue")
	if got := client.Metrics().Snapshot(); got.UDPAssociationRescueFailures != 0 || got.Fallbacks == 0 {
		t.Fatalf("unexpected UDP rescue metrics: %+v", got)
	}

	serviceCancel()
	for range 3 {
		select {
		case <-errorsCh:
		case <-time.After(2 * time.Second):
			t.Fatal("service shutdown timeout")
		}
	}
}

// Blocking UDP is not a one-way mode switch. AUTO has to use TCP while the
// endpoint is in cooldown, then probe QUIC again after the cooldown and clear
// the penalty when that probe succeeds. This covers the blocked/recovered
// sequence without relying on a host firewall or wall-clock minutes.
func TestIntermittentUDPBlockingReturnsToQUIC(t *testing.T) {
	tcpListener, quicPacketConn := listenTCPAndUDPOnOnePort(t)
	defer tcpListener.Close()
	tlsCert, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(ServerConfig{
		ListenAddr: tcpListener.Addr().String(), Credentials: tlsCert,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, EnableQUIC: true,
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: tcpListener.Addr().String(), Credentials: roots, Transport: TransportAuto,
		FallbackDelay: 250 * time.Millisecond, UDPFailureThreshold: 1, UDPCooldown: 150 * time.Millisecond,
		DialTimeout: 2 * time.Second, HandshakeTimeout: 2 * time.Second, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	defer serviceCancel()
	firstQUICCtx, stopFirstQUIC := context.WithCancel(serviceCtx)
	tcpErr := make(chan error, 1)
	firstQUICErr := make(chan error, 1)
	go func() { tcpErr <- server.ServeListener(serviceCtx, tcpListener) }()
	go func() { firstQUICErr <- server.ServePacketConn(firstQUICCtx, quicPacketConn) }()

	open := func(want TransportKind) {
		t.Helper()
		ctx, cancel := context.WithTimeout(serviceCtx, 5*time.Second)
		defer cancel()
		association, err := client.openUDPAssociation(ctx, nil)
		if err != nil {
			t.Fatalf("open UDP association expecting %s: %v", want, err)
		}
		if association.lane.kind != want {
			_ = association.lane.fc.Close()
			t.Fatalf("association transport = %s, want %s", association.lane.kind, want)
		}
		_ = association.lane.fc.Close()
	}

	open(TransportQUIC)
	stopFirstQUIC()
	select {
	case <-firstQUICErr:
	case <-time.After(3 * time.Second):
		t.Fatal("first QUIC listener did not stop")
	}
	open(TransportTCP)

	// Restore UDP on the same endpoint. The next post-cooldown association is
	// the health probe; a successful QUIC authentication makes UDP healthy.
	restartedPacketConn, err := net.ListenPacket("udp", tcpListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	restartedCtx, stopRestarted := context.WithCancel(serviceCtx)
	defer stopRestarted()
	restartedErr := make(chan error, 1)
	go func() { restartedErr <- server.ServePacketConn(restartedCtx, restartedPacketConn) }()
	time.Sleep(250 * time.Millisecond)
	open(TransportQUIC)
	if got := client.Metrics().Snapshot().Fallbacks; got == 0 {
		t.Fatal("blocked interval did not record a TCP fallback")
	}

	serviceCancel()
	for name, errors := range map[string]<-chan error{"TCP": tcpErr, "restarted QUIC": restartedErr} {
		select {
		case <-errors:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s service did not stop", name)
		}
	}
}

func openTestUDPAssociation(t *testing.T, control net.Conn) *net.UDPAddr {
	t.Helper()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(control, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("method response %v err=%v", method, err)
	}
	if _, err := control.Write([]byte{5, socks5.CommandUDPAssociate, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != socks5.ReplySucceeded {
		t.Fatalf("UDP associate failed: %d", reply[1])
	}
	_ = control.SetDeadline(time.Time{})
	return &net.UDPAddr{IP: net.IPv4(reply[4], reply[5], reply[6], reply[7]), Port: int(binary.BigEndian.Uint16(reply[8:10]))}
}
