package pep

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/pathsim"
	"github.com/4fuu/niulang/internal/socks5"
)

type qosExperimentResult struct {
	Mode             string  `json:"mode"`
	Loss             float64 `json:"loss"`
	BurstPackets     float64 `json:"burst_packets"`
	RTTMS            float64 `json:"rtt_ms"`
	RateMbit         float64 `json:"rate_mbit"`
	JitterMS         float64 `json:"jitter_ms"`
	RecoveryAfterMS  float64 `json:"recovery_after_ms,omitempty"`
	BytesBefore      uint64  `json:"bytes_before"`
	BytesAfter       uint64  `json:"bytes_after"`
	FailoverMS       float64 `json:"failover_ms"`
	MaximumReadStall float64 `json:"maximum_read_stall_ms"`
	TCPReady         bool    `json:"tcp_ready"`
	Seed             int64   `json:"seed"`
}

type udpQoSExperimentResult struct {
	Loss            float64 `json:"loss"`
	BurstPackets    float64 `json:"burst_packets"`
	RTTMS           float64 `json:"rtt_ms"`
	RateMbit        float64 `json:"rate_mbit"`
	JitterMS        float64 `json:"jitter_ms"`
	RecoveryAfterMS float64 `json:"recovery_after_ms,omitempty"`
	PayloadBytes    int     `json:"payload_bytes"`
	SentAfter       uint64  `json:"sent_after"`
	ReceivedAfter   uint64  `json:"received_after"`
	DeliveryPercent float64 `json:"delivery_percent"`
	FailoverMS      float64 `json:"failover_ms"`
	LatencyP95MS    float64 `json:"latency_p95_ms"`
	LatencyMaxMS    float64 `json:"latency_max_ms"`
	ControllerErase float64 `json:"controller_erasure"`
	PacketsSent     uint64  `json:"transport_packets_sent"`
	PacketsLost     uint64  `json:"transport_packets_lost"`
	Seed            int64   `json:"seed"`
}

// TestQoSDifferentialRecoveryExperiment is opt-in because it is a timed path
// experiment rather than a unit test. It keeps TCP healthy while stepping a
// live QUIC path into severe loss and emits one JSON record for the campaign
// harness to retain.
func TestQoSDifferentialRecoveryExperiment(t *testing.T) {
	if os.Getenv("NIULANG_QOS_EXPERIMENT") == "" {
		t.Skip("set NIULANG_QOS_EXPERIMENT=1 to run the timed QoS experiment")
	}
	loss := qosExperimentFloat(t, "NIULANG_QOS_LOSS", 0.95)
	burst := qosExperimentFloat(t, "NIULANG_QOS_BURST", 1)
	rttMS := qosExperimentFloat(t, "NIULANG_QOS_RTT_MS", 50)
	rateMbit := qosExperimentFloat(t, "NIULANG_QOS_RATE_MBIT", 50)
	jitterMS := qosExperimentFloat(t, "NIULANG_QOS_JITTER_MS", 0)
	recoveryAfterMS := qosExperimentFloat(t, "NIULANG_QOS_RECOVER_AFTER_MS", 0)
	seed := int64(qosExperimentFloat(t, "NIULANG_QOS_SEED", 771))
	result := runQoSExperiment(t, loss, burst, time.Duration(rttMS*float64(time.Millisecond)), rateMbit, time.Duration(jitterMS*float64(time.Millisecond)), time.Duration(recoveryAfterMS*float64(time.Millisecond)), seed)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
	if os.Getenv("NIULANG_QOS_REQUIRE_FAILOVER") != "0" && (!result.TCPReady || result.FailoverMS <= 0) {
		t.Fatalf("differential recovery did not activate: %+v", result)
	}
}

// TestUDPQoSDifferentialRecoveryExperiment drives application UDP datagrams
// over QUIC, sharply degrades only the outer UDP carrier, and records the
// residual loss and tail latency after the association resumes on its hot TCP
// standby. It is opt-in for the same reason as the byte-stream campaign above.
func TestUDPQoSDifferentialRecoveryExperiment(t *testing.T) {
	if os.Getenv("NIULANG_UDP_QOS_EXPERIMENT") == "" {
		t.Skip("set NIULANG_UDP_QOS_EXPERIMENT=1 to run the timed UDP QoS experiment")
	}
	loss := qosExperimentFloat(t, "NIULANG_QOS_LOSS", 0.95)
	burst := qosExperimentFloat(t, "NIULANG_QOS_BURST", 1)
	rttMS := qosExperimentFloat(t, "NIULANG_QOS_RTT_MS", 50)
	rateMbit := qosExperimentFloat(t, "NIULANG_QOS_RATE_MBIT", 50)
	jitterMS := qosExperimentFloat(t, "NIULANG_QOS_JITTER_MS", 0)
	recoveryAfterMS := qosExperimentFloat(t, "NIULANG_QOS_RECOVER_AFTER_MS", 0)
	payloadBytes := int(qosExperimentFloat(t, "NIULANG_QOS_UDP_PAYLOAD", 256))
	if payloadBytes < 16 || payloadBytes > 65507 {
		t.Fatalf("NIULANG_QOS_UDP_PAYLOAD must be between 16 and 65507, got %d", payloadBytes)
	}
	seed := int64(qosExperimentFloat(t, "NIULANG_QOS_SEED", 771))
	result := runUDPQoSExperiment(t, loss, burst, time.Duration(rttMS*float64(time.Millisecond)), rateMbit, time.Duration(jitterMS*float64(time.Millisecond)), time.Duration(recoveryAfterMS*float64(time.Millisecond)), payloadBytes, seed)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
	if os.Getenv("NIULANG_QOS_REQUIRE_FAILOVER") != "0" && (result.FailoverMS <= 0 || result.ReceivedAfter == 0) {
		t.Fatalf("UDP differential recovery did not activate: %+v", result)
	}
}

func qosExperimentFloat(t *testing.T, name string, fallback float64) float64 {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return parsed
}

func runUDPQoSExperiment(t *testing.T, loss, burst float64, rtt time.Duration, rateMbit float64, jitter, recoveryAfter time.Duration, payloadBytes int, seed int64) udpQoSExperimentResult {
	t.Helper()
	destination, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, readErr := destination.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			_, _ = destination.WriteToUDP(buf[:n], addr)
		}
	}()

	certificate, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverTCP, serverUDP := listenTCPAndUDPOnOnePort(t)
	server, err := NewServer(ServerConfig{
		ListenAddr: serverTCP.Addr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true},
		EnableTCP:         true, EnableQUIC: true, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := pathsim.Config{
		OneWayDelay: rtt / 2, DelayJitter: jitter, LossRate: 0.01,
		LossBurstPackets: burst, RateBytesPerSec: uint64(rateMbit * 1_000_000 / 8),
		QueueBytes: 2 * 1024 * 1024, Seed: seed,
	}
	udpRelay, err := pathsim.New("127.0.0.1:0", serverUDP.LocalAddr().String(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer udpRelay.Close()
	tcpPath := path
	tcpPath.LossRate, tcpPath.UpstreamLossRate = 0, 0
	tcpRelay, err := pathsim.NewTCP(udpRelay.LocalAddr(), serverTCP.Addr().String(), tcpPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpRelay.Close()

	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: udpRelay.LocalAddr(),
		Credentials: roots, Transport: TransportAuto, EnableQUICPool: true,
		FallbackDelay: 300 * time.Millisecond, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- server.ServeListener(ctx, serverTCP) }()
	go func() { errorsCh <- server.ServePacketConn(ctx, serverUDP) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

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
	readyDeadline := time.Now().Add(5 * time.Second)
	for !client.standbyReady(time.Now()) && time.Now().Before(readyDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !client.standbyReady(time.Now()) {
		t.Fatal("TCP standby did not become ready")
	}

	started := time.Now()
	lossAt := started.Add(2 * time.Second)
	endAt := lossAt.Add(7 * time.Second)
	var sentAfter atomic.Uint64
	stopSender := make(chan struct{})
	senderDone := make(chan struct{})
	destinationAddr := destination.LocalAddr().(*net.UDPAddr)
	go func() {
		defer close(senderDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var sequence uint64
		for {
			select {
			case now := <-ticker.C:
				sequence++
				payload := make([]byte, payloadBytes)
				binary.BigEndian.PutUint64(payload[:8], uint64(now.UnixNano()))
				binary.BigEndian.PutUint64(payload[8:16], sequence)
				request := []byte{0, 0, 0, 1}
				request = append(request, destinationAddr.IP.To4()...)
				var port [2]byte
				binary.BigEndian.PutUint16(port[:], uint16(destinationAddr.Port))
				request = append(request, port[:]...)
				request = append(request, payload...)
				if !now.Before(lossAt) {
					sentAfter.Add(1)
				}
				_, _ = udpClient.WriteToUDP(request, bound)
			case <-stopSender:
				return
			}
		}
	}()

	lossApplied := false
	pathRecovered := false
	failoverAt := time.Time{}
	latencies := make([]float64, 0, 256)
	var receivedAfter uint64
	buf := make([]byte, 65535)
	for time.Now().Before(endAt) {
		now := time.Now()
		if !lossApplied && !now.Before(lossAt) {
			udpRelay.SetLossRate(loss, loss)
			lossApplied = true
		}
		if lossApplied && !pathRecovered && recoveryAfter > 0 && !now.Before(lossAt.Add(recoveryAfter)) {
			udpRelay.SetLossRate(0.01, 0.01)
			pathRecovered = true
		}
		if failoverAt.IsZero() && client.Metrics().Snapshot().QUICDegradationFailovers > 0 {
			failoverAt = now
		}
		_ = udpClient.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, _, readErr := udpClient.ReadFromUDP(buf)
		if readErr != nil {
			continue
		}
		packet, err := socks5.ReadUDPDatagram(buf[:n])
		if err != nil || len(packet.Payload) < 16 {
			continue
		}
		sentAt := time.Unix(0, int64(binary.BigEndian.Uint64(packet.Payload[:8])))
		if !sentAt.Before(lossAt) {
			receivedAfter++
			latencies = append(latencies, float64(time.Since(sentAt))/float64(time.Millisecond))
		}
	}
	close(stopSender)
	<-senderDone
	sort.Float64s(latencies)
	result := udpQoSExperimentResult{
		Loss: loss, BurstPackets: burst, RTTMS: float64(rtt) / float64(time.Millisecond),
		RateMbit: rateMbit, JitterMS: float64(jitter) / float64(time.Millisecond), RecoveryAfterMS: float64(recoveryAfter) / float64(time.Millisecond),
		PayloadBytes: payloadBytes, SentAfter: sentAfter.Load(), ReceivedAfter: receivedAfter, Seed: seed,
	}
	if result.SentAfter > 0 {
		result.DeliveryPercent = 100 * float64(result.ReceivedAfter) / float64(result.SentAfter)
	}
	if !failoverAt.IsZero() {
		result.FailoverMS = float64(failoverAt.Sub(lossAt)) / float64(time.Millisecond)
	}
	if len(latencies) > 0 {
		result.LatencyP95MS = latencies[(95*len(latencies)-1)/100]
		result.LatencyMaxMS = latencies[len(latencies)-1]
	}
	client.quicMu.Lock()
	if client.quicGeneration != nil && client.quicGeneration.controller != nil {
		result.ControllerErase = client.quicGeneration.controller.Telemetry().Erasure
	}
	if client.quicGeneration != nil && client.quicGeneration.conn != nil {
		stats := client.quicGeneration.conn.ConnectionStats()
		result.PacketsSent, result.PacketsLost = stats.PacketsSent, stats.PacketsLost
	}
	client.quicMu.Unlock()

	cancel()
	_ = control.Close()
	for range 3 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
	return result
}

func runQoSExperiment(t *testing.T, loss, burst float64, rtt time.Duration, rateMbit float64, jitter, recoveryAfter time.Duration, seed int64) qosExperimentResult {
	t.Helper()
	destination, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	go serveByteSource(destination)

	certificate, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverTCP, serverUDP := listenTCPAndUDPOnOnePort(t)
	server, err := NewServer(ServerConfig{
		ListenAddr: serverTCP.Addr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true},
		EnableTCP:         true, EnableQUIC: true, ChunkSize: 16 * 1024, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := pathsim.Config{
		OneWayDelay: rtt / 2, DelayJitter: jitter, LossRate: 0.01,
		LossBurstPackets: burst, RateBytesPerSec: uint64(rateMbit * 1_000_000 / 8),
		QueueBytes: 2 * 1024 * 1024, Seed: seed,
	}
	udpRelay, err := pathsim.New("127.0.0.1:0", serverUDP.LocalAddr().String(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer udpRelay.Close()
	tcpPath := path
	tcpPath.LossRate, tcpPath.UpstreamLossRate = 0, 0
	tcpRelay, err := pathsim.NewTCP(udpRelay.LocalAddr(), serverTCP.Addr().String(), tcpPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpRelay.Close()

	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: udpRelay.LocalAddr(),
		Credentials: roots, Transport: TransportAuto, EnableQUICPool: true,
		FallbackDelay: 300 * time.Millisecond, ChunkSize: 16 * 1024, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- server.ServeListener(ctx, serverTCP) }()
	go func() { errorsCh <- server.ServePacketConn(ctx, serverUDP) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn := dialTestSOCKS(t, clientListener.Addr().String(), destination.Addr().String())
	defer conn.Close()
	readyDeadline := time.Now().Add(5 * time.Second)
	for !client.standbyReady(time.Now()) && time.Now().Before(readyDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !client.standbyReady(time.Now()) {
		t.Fatal("TCP standby did not become ready")
	}

	started := time.Now()
	lossAt := started.Add(2 * time.Second)
	endAt := lossAt.Add(7 * time.Second)
	var before, after uint64
	lastRead := started
	maxStall := time.Duration(0)
	failoverAt := time.Time{}
	buf := make([]byte, 64*1024)
	lossApplied := false
	pathRecovered := false
	for time.Now().Before(endAt) {
		now := time.Now()
		if !lossApplied && !now.Before(lossAt) {
			udpRelay.SetLossRate(loss, loss)
			lossApplied = true
		}
		if lossApplied && !pathRecovered && recoveryAfter > 0 && !now.Before(lossAt.Add(recoveryAfter)) {
			udpRelay.SetLossRate(0.01, 0.01)
			pathRecovered = true
		}
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, readErr := conn.Read(buf)
		now = time.Now()
		if n > 0 {
			stall := now.Sub(lastRead)
			if stall > maxStall {
				maxStall = stall
			}
			lastRead = now
			if now.Before(lossAt) {
				before += uint64(n)
			} else {
				after += uint64(n)
			}
		}
		if failoverAt.IsZero() && serverUsingTCPRecovery(server) {
			failoverAt = now
		}
		if readErr != nil {
			var timeout net.Error
			if !errors.As(readErr, &timeout) || !timeout.Timeout() {
				break
			}
		}
	}
	if terminalStall := time.Since(lastRead); terminalStall > maxStall {
		maxStall = terminalStall
	}
	_ = conn.SetReadDeadline(time.Time{})
	result := qosExperimentResult{
		Mode: "handoff", Loss: loss, BurstPackets: burst,
		RTTMS: float64(rtt) / float64(time.Millisecond), RateMbit: rateMbit,
		JitterMS: float64(jitter) / float64(time.Millisecond), RecoveryAfterMS: float64(recoveryAfter) / float64(time.Millisecond),
		BytesBefore: before, BytesAfter: after,
		MaximumReadStall: float64(maxStall) / float64(time.Millisecond),
		TCPReady:         !failoverAt.IsZero(), Seed: seed,
	}
	if !failoverAt.IsZero() {
		result.FailoverMS = float64(failoverAt.Sub(lossAt)) / float64(time.Millisecond)
	}

	cancel()
	_ = conn.Close()
	for range 3 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
	return result
}

func serverUsingTCPRecovery(server *Server) bool {
	server.sessionsMu.RLock()
	defer server.sessionsMu.RUnlock()
	for _, flow := range server.sessions {
		return flow.tcpMode && hasTCPLane(flow.flow)
	}
	return false
}

func serveByteSource(listener net.Listener) {
	payload := make([]byte, 64*1024)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			for {
				if _, err := conn.Write(payload); err != nil {
					return
				}
			}
		}()
	}
}
