// Command wanoptbench runs wanopt and a TUIC-shaped reference proxy over an
// identical, deterministic emulated WAN path and reports goodput and latency.
//
// The live China-US link that this project targets moves between roughly 0%
// and 50% packet loss within minutes, which makes sequential live A/B trials
// unable to separate a transport regression from a path window. This harness
// therefore runs both stacks in one process against a seeded path emulator, so
// a difference between the two rows is attributable to the transports.
//
// Live-path campaigns remain necessary and are not replaced by this tool; it
// is the fast, repeatable inner loop.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/icourses-dev/wanopt/internal/baseline"
	"github.com/icourses-dev/wanopt/internal/pathsim"
	"github.com/icourses-dev/wanopt/internal/pep"
)

type options struct {
	stacks       string
	rttMillis    int
	lossPercent  float64
	lossBurst    float64
	lossUp       float64
	rateMbits    float64
	perFlowMbits float64
	queueBytes   int
	seed         int64
	bytes        int64
	trials       int
	flows        string
	congestion   string
	brutalMbits  float64
	lanes        int
	chunkSize    int
	initialLanes int
	quicPool     bool
	timeout      time.Duration
	cpuProfile   string
	verbose      bool
	latency      bool
	interactive  bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "wanoptbench: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var opts options
	fs := flag.NewFlagSet("wanoptbench", flag.ContinueOnError)
	fs.StringVar(&opts.stacks, "stacks", "baseline,wanopt", "comma-separated stacks to measure")
	fs.IntVar(&opts.rttMillis, "rtt", 200, "emulated round-trip time in milliseconds")
	fs.Float64Var(&opts.lossPercent, "loss", 0, "per-packet loss percentage in each direction")
	fs.Float64Var(&opts.lossBurst, "loss-burst", 0, "mean loss burst length in packets (0 or 1 gives independent loss)")
	fs.Float64Var(&opts.lossUp, "loss-up", 0, "client-to-server loss percentage, overriding --loss for that direction")
	fs.Float64Var(&opts.rateMbits, "rate", 100, "bottleneck rate in Mbit/s in each direction (0 disables)")
	fs.Float64Var(&opts.perFlowMbits, "per-flow-rate", 0, "per-source-address rate in Mbit/s, modelling per-flow policing (0 disables)")
	fs.IntVar(&opts.queueBytes, "queue", 0, "bottleneck queue in bytes (0 selects one BDP)")
	fs.Int64Var(&opts.seed, "seed", 1, "path emulator seed")
	fs.Int64Var(&opts.bytes, "bytes", 10<<20, "object size per flow in bytes")
	fs.IntVar(&opts.trials, "trials", 3, "trials per stack")
	fs.StringVar(&opts.flows, "flows", "1", "comma-separated concurrent flow counts")
	fs.StringVar(&opts.congestion, "congestion", "bbr-tuic", "congestion controller for both stacks")
	fs.Float64Var(&opts.brutalMbits, "brutal-rate", 0, "wanopt fixed send rate in Mbit/s when --congestion=brutal")
	fs.IntVar(&opts.lanes, "lanes", 1, "wanopt maximum lanes")
	fs.IntVar(&opts.chunkSize, "chunk", 0, "wanopt data frame size in bytes (0 selects the default)")
	fs.IntVar(&opts.initialLanes, "initial-lanes", 1, "wanopt lanes opened before SOCKS CONNECT succeeds")
	fs.BoolVar(&opts.quicPool, "quic-pool", true, "enable the wanopt pooled QUIC connection")
	fs.DurationVar(&opts.timeout, "timeout", 120*time.Second, "per-trial timeout")
	fs.StringVar(&opts.cpuProfile, "cpuprofile", "", "write a CPU profile to this path")
	fs.BoolVar(&opts.verbose, "verbose", false, "log transport diagnostics")
	fs.BoolVar(&opts.latency, "latency", false, "also measure small-request latency")
	fs.BoolVar(&opts.interactive, "interactive", false, "issue small requests during the bulk transfer and report their latency")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if opts.trials <= 0 || opts.bytes <= 0 {
		return errors.New("trials and bytes must be positive")
	}

	if opts.cpuProfile != "" {
		file, err := os.Create(opts.cpuProfile)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := pprof.StartCPUProfile(file); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}

	flowCounts, err := parseCounts(opts.flows)
	if err != nil {
		return err
	}
	pathCfg := pathsim.Config{
		OneWayDelay:            time.Duration(opts.rttMillis) * time.Millisecond / 2,
		LossRate:               opts.lossPercent / 100,
		LossBurstPackets:       opts.lossBurst,
		UpstreamLossRate:       opts.lossUp / 100,
		RateBytesPerSec:        uint64(opts.rateMbits * 1e6 / 8),
		PerFlowRateBytesPerSec: uint64(opts.perFlowMbits * 1e6 / 8),
		QueueBytes:             opts.queueBytes,
		Seed:                   opts.seed,
	}

	origin, err := newOrigin(opts.bytes)
	if err != nil {
		return err
	}
	defer origin.Close()

	fmt.Printf("# path rtt=%dms loss=%.2f%% burst=%.1f rate=%.1fMbit/s per_flow=%.1fMbit/s queue=%s seed=%d object=%s congestion=%s lanes=%d\n",
		opts.rttMillis, opts.lossPercent, opts.lossBurst, opts.rateMbits, opts.perFlowMbits, humanQueue(pathCfg), opts.seed,
		humanBytes(opts.bytes), opts.congestion, opts.lanes)
	fmt.Printf("stack\tflows\ttrial\tseconds\tmbits_per_sec\tcomplete\tnote\n")

	for _, stack := range strings.Split(opts.stacks, ",") {
		stack = strings.TrimSpace(stack)
		if stack == "" {
			continue
		}
		for _, flows := range flowCounts {
			for trial := 1; trial <= opts.trials; trial++ {
				// Each trial gets a fresh emulator and fresh proxy processes so
				// no trial inherits another trial's congestion state or queue.
				result := measure(stack, opts, pathCfg, origin, flows, trial)
				fmt.Printf("%s\t%d\t%d\t%.3f\t%.3f\t%d\t%s\n",
					stack, flows, trial, result.seconds, result.mbitsPerSec, boolInt(result.complete), result.note)
			}
		}
	}
	if opts.latency {
		if err := measureLatency(opts, pathCfg, origin); err != nil {
			return err
		}
	}
	return nil
}

type trialResult struct {
	seconds     float64
	mbitsPerSec float64
	complete    bool
	note        string
}

func measure(stack string, opts options, pathCfg pathsim.Config, origin *origin, flows, trial int) trialResult {
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	// Seeding per trial keeps trials independent while remaining reproducible
	// for a given (seed, trial) pair.
	cfg := pathCfg
	cfg.Seed = pathCfg.Seed + int64(trial)*1000

	harness, err := startStack(ctx, stack, opts, cfg)
	if err != nil {
		return trialResult{note: "setup: " + err.Error()}
	}
	defer harness.Close()

	// One warm-up request establishes the QUIC connection so the measured
	// transfer reflects steady-state transport behavior for both stacks. TUIC
	// keeps a persistent connection, so charging wanopt for a cold handshake
	// and not TUIC would be the wrong comparison; handshake cost is reported
	// separately by the latency mode.
	if err := warmUp(ctx, harness.socks, origin); err != nil {
		return trialResult{note: "warmup: " + err.Error()}
	}

	var wg sync.WaitGroup
	results := make([]int64, flows)
	errs := make([]error, flows)
	// The scheduler's whole purpose is that a bulk transfer must not push
	// interactive latency past its budget, so measure that directly rather
	// than inferring it from throughput.
	probeStop := make(chan struct{})
	probeDone := make(chan []requestStages, 1)
	if opts.interactive {
		go func() { probeDone <- probeInteractive(ctx, harness.socks, origin, probeStop) }()
	}
	started := time.Now()
	for i := range flows {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			n, fetchErr := fetch(ctx, harness.socks, origin.addr, opts.bytes)
			results[index], errs[index] = n, fetchErr
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(started)
	var probes []requestStages
	if opts.interactive {
		close(probeStop)
		probes = <-probeDone
	}

	var total int64
	complete := true
	note := ""
	for i := range flows {
		total += results[i]
		if errs[i] != nil {
			complete = false
			if note == "" {
				note = errs[i].Error()
			}
		} else if results[i] != opts.bytes {
			complete = false
			if note == "" {
				note = fmt.Sprintf("short body %d", results[i])
			}
		}
	}
	up, down := harness.relay.Stats()
	if note == "" {
		note = fmt.Sprintf("udp_up=%d/%d udp_down=%d/%d", up.PacketsOut, up.PacketsIn, down.PacketsOut, down.PacketsIn)
	}
	if opts.interactive {
		note = summarizeProbes(probes) + " " + note
	}
	return trialResult{
		seconds:     elapsed.Seconds(),
		mbitsPerSec: float64(total) * 8 / elapsed.Seconds() / 1e6,
		complete:    complete,
		note:        note,
	}
}

// probeInteractive issues one small request at a time until stopped, and
// returns each request's latency. Failures are recorded as the elapsed time so
// a stalled probe cannot be silently dropped from the distribution.
func probeInteractive(ctx context.Context, socksAddr string, o *origin, stop <-chan struct{}) []requestStages {
	var samples []requestStages
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return samples
		case <-ctx.Done():
			return samples
		case <-ticker.C:
		}
		_, stages, err := fetchTimed(ctx, socksAddr, o.smallAddr, o.smallSize)
		if err != nil && ctx.Err() != nil {
			return samples
		}
		samples = append(samples, stages)
	}
}

func summarizeProbes(samples []requestStages) string {
	if len(samples) == 0 {
		return "interactive=none"
	}
	quantile := func(pick func(requestStages) time.Duration, q float64) float64 {
		values := make([]time.Duration, len(samples))
		for i, sample := range samples {
			values[i] = pick(sample)
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return float64(values[int(q*float64(len(values)-1))].Microseconds()) / 1000
	}
	total := func(s requestStages) time.Duration { return s.Total }
	connect := func(s requestStages) time.Duration { return s.Connect }
	first := func(s requestStages) time.Duration { return s.FirstByte }
	return fmt.Sprintf("interactive_n=%d p50=%.0fms p95=%.0fms max=%.0fms connect_p95=%.0fms firstbyte_p95=%.0fms",
		len(samples), quantile(total, 0.5), quantile(total, 0.95), quantile(total, 1),
		quantile(connect, 0.95), quantile(first, 0.95))
}

func measureLatency(opts options, pathCfg pathsim.Config, origin *origin) error {
	fmt.Printf("\nstack\ttrial\tconnect_ms\trequest_ms\tnote\n")
	for _, stack := range strings.Split(opts.stacks, ",") {
		stack = strings.TrimSpace(stack)
		if stack == "" {
			continue
		}
		for trial := 1; trial <= opts.trials; trial++ {
			ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
			cfg := pathCfg
			cfg.Seed = pathCfg.Seed + int64(trial)*1000
			harness, err := startStack(ctx, stack, opts, cfg)
			if err != nil {
				fmt.Printf("%s\t%d\t\t\tsetup: %v\n", stack, trial, err)
				cancel()
				continue
			}
			// Cold request: includes the outer handshake for both stacks.
			coldStart := time.Now()
			_, coldErr := fetch(ctx, harness.socks, origin.smallAddr, origin.smallSize)
			cold := time.Since(coldStart)
			// Warm request: the outer connection already exists.
			warmStart := time.Now()
			_, warmErr := fetch(ctx, harness.socks, origin.smallAddr, origin.smallSize)
			warm := time.Since(warmStart)
			note := ""
			if coldErr != nil {
				note = "cold: " + coldErr.Error()
			} else if warmErr != nil {
				note = "warm: " + warmErr.Error()
			}
			fmt.Printf("%s\t%d\t%.1f\t%.1f\t%s\n", stack, trial, float64(cold.Microseconds())/1000, float64(warm.Microseconds())/1000, note)
			harness.Close()
			cancel()
		}
	}
	return nil
}

// ---------------------------------------------------------------- harness ---

type harness struct {
	socks  string
	relay  *pathsim.Relay
	closes []func()
}

func (h *harness) Close() {
	for i := len(h.closes) - 1; i >= 0; i-- {
		h.closes[i]()
	}
}

func startStack(ctx context.Context, stack string, opts options, pathCfg pathsim.Config) (*harness, error) {
	certificate, roots, err := selfSignedCertificate()
	if err != nil {
		return nil, err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if opts.verbose {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	serverPacket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	relay, err := pathsim.New("127.0.0.1:0", serverPacket.LocalAddr().String(), pathCfg)
	if err != nil {
		_ = serverPacket.Close()
		return nil, err
	}
	socksListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = relay.Close()
		_ = serverPacket.Close()
		return nil, err
	}
	h := &harness{socks: socksListener.Addr().String(), relay: relay}
	h.closes = append(h.closes, func() { _ = socksListener.Close() }, func() { _ = relay.Close() }, func() { _ = serverPacket.Close() })

	switch stack {
	case "baseline", "tuic":
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			h.Close()
			return nil, err
		}
		server, err := baseline.NewServer(baseline.ServerConfig{
			ListenAddr: serverPacket.LocalAddr().String(), Certificate: certificate,
			Token: token, Transport: baseline.TUICTransport(),
			Congestion:        baseline.CongestionKind(opts.congestion),
			BrutalBytesPerSec: uint64(opts.brutalMbits * 1e6 / 8), Logger: logger,
		})
		if err != nil {
			h.Close()
			return nil, err
		}
		client, err := baseline.NewClient(baseline.ClientConfig{
			ListenAddr: h.socks, RemoteAddr: relay.LocalAddr(), ServerName: "wanopt.test",
			RootCAs: roots, Token: token, Transport: baseline.TUICTransport(),
			Congestion:        baseline.CongestionKind(opts.congestion),
			BrutalBytesPerSec: uint64(opts.brutalMbits * 1e6 / 8), Logger: logger,
		})
		if err != nil {
			h.Close()
			return nil, err
		}
		go func() { _ = server.Serve(ctx, serverPacket) }()
		go func() { _ = client.ServeListener(ctx, socksListener) }()
		h.closes = append(h.closes, client.Close)
	case "wanopt":
		secret := []byte("wanoptbench-shared-secret-32bytes!")
		server, err := pep.NewServer(pep.ServerConfig{
			ListenAddr: serverPacket.LocalAddr().String(), Certificate: certificate, Secret: secret,
			DestinationPolicy: pep.DestinationPolicy{AllowPrivate: true}, EnableQUIC: true, ChunkSize: opts.chunkSize,
			Congestion: pep.CongestionControlKind(opts.congestion), BrutalBytesPerSec: uint64(opts.brutalMbits * 1e6 / 8),
			MaxLanes: opts.lanes, Logger: logger,
		})
		if err != nil {
			h.Close()
			return nil, err
		}
		client, err := pep.NewClient(pep.ClientConfig{
			ListenAddr: h.socks, RemoteAddr: relay.LocalAddr(), ServerName: "wanopt.test",
			Secret: secret, RootCAs: roots, Transport: pep.TransportQUIC, ChunkSize: opts.chunkSize,
			EnableQUICPool: opts.quicPool, OptimisticOpen: true,
			Congestion:        pep.CongestionControlKind(opts.congestion),
			BrutalBytesPerSec: uint64(opts.brutalMbits * 1e6 / 8),
			InitialLanes:      opts.initialLanes, MaxLanes: opts.lanes, Logger: logger,
		})
		if err != nil {
			h.Close()
			return nil, err
		}
		go func() { _ = server.ServePacketConn(ctx, serverPacket) }()
		go func() { _ = client.ServeListener(ctx, socksListener) }()
	default:
		h.Close()
		return nil, fmt.Errorf("unknown stack %q", stack)
	}
	return h, nil
}

// ----------------------------------------------------------------- origin ---

// origin is a local HTTP-free byte source. Using a raw TCP responder rather
// than net/http keeps the measurement free of HTTP parsing overhead and makes
// the expected byte count exact.
type origin struct {
	listener      net.Listener
	smallListener net.Listener
	addr          string
	smallAddr     string
	smallSize     int64
	size          int64
}

func newOrigin(size int64) (*origin, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	smallListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	o := &origin{
		listener: listener, smallListener: smallListener,
		addr: listener.Addr().String(), smallAddr: smallListener.Addr().String(),
		size: size, smallSize: 1024,
	}
	go o.serve(listener, size)
	go o.serve(smallListener, o.smallSize)
	return o, nil
}

func (o *origin) serve(listener net.Listener, size int64) {
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			// Wait for the one-byte request so the response is not sent before
			// the client is ready; this mirrors an HTTP request/response.
			var request [1]byte
			if _, err := io.ReadFull(conn, request[:]); err != nil {
				return
			}
			remaining := size
			for remaining > 0 {
				chunk := int64(len(payload))
				if chunk > remaining {
					chunk = remaining
				}
				n, err := conn.Write(payload[:chunk])
				if err != nil {
					return
				}
				remaining -= int64(n)
			}
		}()
	}
}

func (o *origin) Close() {
	_ = o.listener.Close()
	_ = o.smallListener.Close()
}

// ------------------------------------------------------------------ SOCKS ---

func warmUp(ctx context.Context, socksAddr string, o *origin) error {
	_, err := fetch(ctx, socksAddr, o.smallAddr, o.smallSize)
	return err
}

func fetch(ctx context.Context, socksAddr, destination string, expect int64) (int64, error) {
	received, _, err := fetchTimed(ctx, socksAddr, destination, expect)
	return received, err
}

// requestStages breaks one request into the parts a transport controls
// separately, so a latency regression can be attributed to flow setup, to the
// first byte, or to the transfer itself rather than only observed in total.
type requestStages struct {
	Connect   time.Duration // SOCKS CONNECT acknowledged
	FirstByte time.Duration // first response byte after the request was written
	Total     time.Duration
}

func fetchTimed(ctx context.Context, socksAddr, destination string, expect int64) (int64, requestStages, error) {
	var stages requestStages
	started := time.Now()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", socksAddr)
	if err != nil {
		return 0, stages, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := socksConnect(conn, destination); err != nil {
		return 0, stages, err
	}
	stages.Connect = time.Since(started)
	if _, err := conn.Write([]byte{'g'}); err != nil {
		return 0, stages, err
	}
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		return 0, stages, err
	}
	stages.FirstByte = time.Since(started)
	received, err := io.Copy(io.Discard, io.LimitReader(conn, expect-1))
	received++
	stages.Total = time.Since(started)
	if err != nil {
		return received, stages, err
	}
	if received != expect {
		return received, stages, fmt.Errorf("received %d of %d bytes", received, expect)
	}
	return received, stages, nil
}

func socksConnect(conn net.Conn, destination string) error {
	host, port, err := net.SplitHostPort(destination)
	if err != nil {
		return err
	}
	portNumber, err := parsePort(port)
	if err != nil {
		return err
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		return err
	}
	if method[0] != 5 || method[1] != 0 {
		return errors.New("SOCKS5 method negotiation failed")
	}
	request := []byte{5, 1, 0, 3, byte(len(host))}
	request = append(request, host...)
	request = append(request, byte(portNumber>>8), byte(portNumber))
	if _, err := conn.Write(request); err != nil {
		return err
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[1] != 0 {
		return fmt.Errorf("SOCKS5 connect failed with code %d", reply[1])
	}
	var skip int
	switch reply[3] {
	case 1:
		skip = 4 + 2
	case 4:
		skip = 16 + 2
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return err
		}
		skip = int(length[0]) + 2
	default:
		return errors.New("unsupported SOCKS5 bound address type")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip)); err != nil {
		return err
	}
	return nil
}

// ------------------------------------------------------------------ utils ---

func parsePort(port string) (int, error) {
	value := 0
	if port == "" {
		return 0, errors.New("empty port")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid port %q", port)
		}
		value = value*10 + int(r-'0')
	}
	if value == 0 || value > 65535 {
		return 0, fmt.Errorf("port %q out of range", port)
	}
	return value, nil
}

func parseCounts(spec string) ([]int, error) {
	parts := strings.Split(spec, ",")
	counts := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value, err := parsePort(part)
		if err != nil {
			return nil, fmt.Errorf("invalid flow count %q", part)
		}
		counts = append(counts, value)
	}
	if len(counts) == 0 {
		return nil, errors.New("no flow counts provided")
	}
	sort.Ints(counts)
	return counts, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func humanQueue(cfg pathsim.Config) string {
	if cfg.QueueBytes > 0 {
		return humanBytes(int64(cfg.QueueBytes))
	}
	return "1BDP"
}

func selfSignedCertificate() (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wanopt.test"},
		DNSNames:     []string{"wanopt.test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		return tls.Certificate{}, nil, errors.New("append benchmark certificate")
	}
	return certificate, roots, nil
}
