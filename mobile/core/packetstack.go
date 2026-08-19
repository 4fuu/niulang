package mobilecore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	defaultMTU          = 1280
	maximumMTU          = 9000
	defaultMaxSessions  = 128
	linkQueueLength     = 64
	copyBufferSize      = 8 * 1024
	udpPacketBufferSize = 4 * 1024
	udpIdleTimeout      = 2 * time.Minute
)

const (
	endpointBufferMinimum = 4 * 1024
	endpointBufferDefault = 32 * 1024
	endpointBufferMaximum = 64 * 1024
)

type packetStack struct {
	ctx       context.Context
	cancel    context.CancelFunc
	tun       io.ReadWriteCloser
	offset    int
	mtu       int
	proxy     socksClient
	stack     *stack.Stack
	link      *channel.Endpoint
	admission chan struct{}
	log       func(level, message string)

	wg        sync.WaitGroup
	closeOnce sync.Once
	firstErr  chan error

	packetsIn       atomic.Uint64
	packetsOut      atomic.Uint64
	malformed       atomic.Uint64
	sessionRejected atomic.Uint64
}

type packetStackSnapshot struct {
	PacketsIn       uint64 `json:"packets_in"`
	PacketsOut      uint64 `json:"packets_out"`
	Malformed       uint64 `json:"malformed_packets"`
	SessionRejected uint64 `json:"sessions_rejected"`
}

func newPacketStack(parent context.Context, tunFD, packetOffset, mtu, maxSessions int, proxy socksClient, log func(string, string)) (*packetStack, error) {
	if tunFD < 0 {
		return nil, errors.New("TUN file descriptor must be non-negative")
	}
	if err := validatePacketStackConfig(packetOffset, mtu, maxSessions); err != nil {
		return nil, err
	}
	duplicate, err := unix.Dup(tunFD)
	if err != nil {
		return nil, fmt.Errorf("duplicate TUN descriptor: %w", err)
	}
	if err := unix.SetNonblock(duplicate, true); err != nil {
		_ = unix.Close(duplicate)
		return nil, fmt.Errorf("configure TUN descriptor: %w", err)
	}
	return newPacketStackWithDevice(parent, os.NewFile(uintptr(duplicate), "queqiao-tun"), packetOffset, mtu, maxSessions, proxy, log)
}

func newPacketStackWithDevice(parent context.Context, device io.ReadWriteCloser, packetOffset, mtu, maxSessions int, proxy socksClient, log func(string, string)) (*packetStack, error) {
	if device == nil {
		return nil, errors.New("packet device is required")
	}
	if err := validatePacketStackConfig(packetOffset, mtu, maxSessions); err != nil {
		_ = device.Close()
		return nil, err
	}
	if mtu == 0 {
		mtu = defaultMTU
	}
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	ctx, cancel := context.WithCancel(parent)
	p := &packetStack{
		ctx: ctx, cancel: cancel, tun: device,
		offset: packetOffset, mtu: mtu, proxy: proxy, admission: make(chan struct{}, maxSessions),
		log: log, firstErr: make(chan error, 1),
	}
	if p.log == nil {
		p.log = func(string, string) {}
	}
	if err := p.initialize(); err != nil {
		_ = p.tun.Close()
		cancel()
		return nil, err
	}
	return p, nil
}

func validatePacketStackConfig(packetOffset, mtu, maxSessions int) error {
	if packetOffset != 0 && packetOffset != 4 {
		return errors.New("packet offset must be 0 or 4")
	}
	if mtu != 0 && (mtu < header.IPv6MinimumMTU || mtu > maximumMTU) {
		return fmt.Errorf("MTU must be between %d and %d", header.IPv6MinimumMTU, maximumMTU)
	}
	if maxSessions > 65535 {
		return errors.New("maximum session count exceeds 65535")
	}
	return nil
}

func (p *packetStack) initialize() error {
	// Configuration validation limits MTU to 9000 before initialization.
	linkMTU := uint32(p.mtu) // #nosec G115 -- validated to [1280, 9000].
	p.link = channel.New(linkQueueLength, linkMTU, "")
	p.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	// gVisor's desktop defaults are generous per endpoint. With dozens of
	// mobile flows they become the dominant multiplicative allocation, so keep
	// TCP and generic socket buffers within a small, explicit range. Full
	// buffers naturally advertise a smaller TCP receive window and backpressure
	// the application instead of growing the process.
	if err := p.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPSendBufferSizeRangeOption{
		Min: endpointBufferMinimum, Default: endpointBufferDefault, Max: endpointBufferMaximum,
	}); err != nil {
		return fmt.Errorf("bound TCP send buffers: %s", err)
	}
	if err := p.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPReceiveBufferSizeRangeOption{
		Min: endpointBufferMinimum, Default: endpointBufferDefault, Max: endpointBufferMaximum,
	}); err != nil {
		return fmt.Errorf("bound TCP receive buffers: %s", err)
	}
	moderateReceive := tcpip.TCPModerateReceiveBufferOption(false)
	if err := p.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &moderateReceive); err != nil {
		return fmt.Errorf("disable TCP receive-buffer growth: %s", err)
	}
	if err := p.stack.SetOption(tcpip.SendBufferSizeOption{
		Min: endpointBufferMinimum, Default: endpointBufferDefault, Max: endpointBufferMaximum,
	}); err != nil {
		return fmt.Errorf("bound socket send buffers: %s", err)
	}
	if err := p.stack.SetOption(tcpip.ReceiveBufferSizeOption{
		Min: endpointBufferMinimum, Default: endpointBufferDefault, Max: endpointBufferMaximum,
	}); err != nil {
		return fmt.Errorf("bound socket receive buffers: %s", err)
	}
	nicID := p.stack.NextNICID()
	if err := p.stack.CreateNIC(nicID, p.link); err != nil {
		return fmt.Errorf("create virtual network interface: %s", err)
	}
	if err := p.stack.SetPromiscuousMode(nicID, true); err != nil {
		return fmt.Errorf("enable virtual interface promiscuous mode: %s", err)
	}
	if err := p.stack.SetSpoofing(nicID, true); err != nil {
		return fmt.Errorf("enable virtual interface address spoofing: %s", err)
	}
	p.stack.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})
	tcpForwarder := tcp.NewForwarder(p.stack, 0, cap(p.admission), p.forwardTCP)
	udpForwarder := udp.NewForwarder(p.stack, p.forwardUDP)
	p.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
	p.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)
	return nil
}

func (p *packetStack) start() {
	p.wg.Add(2)
	go p.pumpTUNToStack()
	go p.pumpStackToTUN()
}

func (p *packetStack) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		_ = p.tun.Close()
		p.link.Close()
		p.stack.Close()
	})
	p.wg.Wait()
	p.stack.Wait()
	select {
	case err := <-p.firstErr:
		return err
	default:
		return nil
	}
}

func (p *packetStack) fail(err error) {
	if err == nil || p.ctx.Err() != nil {
		return
	}
	select {
	case p.firstErr <- err:
	default:
	}
	p.log("error", err.Error())
	p.cancel()
	_ = p.tun.Close()
}

func (p *packetStack) snapshot() packetStackSnapshot {
	return packetStackSnapshot{
		PacketsIn: p.packetsIn.Load(), PacketsOut: p.packetsOut.Load(),
		Malformed: p.malformed.Load(), SessionRejected: p.sessionRejected.Load(),
	}
}

func (p *packetStack) pumpTUNToStack() {
	defer p.wg.Done()
	packet := make([]byte, p.offset+p.mtu)
	for {
		n, err := p.tun.Read(packet)
		if err != nil {
			if p.ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return
			}
			p.fail(fmt.Errorf("read TUN packet: %w", err))
			return
		}
		if n <= p.offset {
			p.malformed.Add(1)
			continue
		}
		payload := packet[p.offset:n]
		protocol, ok := packetProtocol(payload)
		if !ok || !p.validPlatformHeader(packet[:p.offset], protocol) {
			p.malformed.Add(1)
			continue
		}
		bufferCopy := append([]byte(nil), payload...)
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(bufferCopy)})
		p.link.InjectInbound(protocol, pkt)
		pkt.DecRef()
		p.packetsIn.Add(1)
	}
}

func (p *packetStack) pumpStackToTUN() {
	defer p.wg.Done()
	packet := make([]byte, p.offset+p.mtu)
	for {
		pkt := p.link.ReadContext(p.ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		payload := view.AsSlice()
		protocol, ok := packetProtocol(payload)
		if !ok {
			p.malformed.Add(1)
			view.Release()
			pkt.DecRef()
			continue
		}
		if len(payload) > p.mtu {
			p.malformed.Add(1)
			view.Release()
			pkt.DecRef()
			continue
		}
		out := packet[:p.offset+len(payload)]
		if p.offset == 4 {
			family := uint32(unix.AF_INET)
			if protocol == ipv6.ProtocolNumber {
				family = unix.AF_INET6
			}
			binary.BigEndian.PutUint32(out[:4], family)
		}
		copy(out[p.offset:], payload)
		view.Release()
		pkt.DecRef()
		if err := writeFull(p.tun, out); err != nil {
			if p.ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return
			}
			p.fail(fmt.Errorf("write TUN packet: %w", err))
			return
		}
		p.packetsOut.Add(1)
	}
}

func packetProtocol(packet []byte) (tcpip.NetworkProtocolNumber, bool) {
	if len(packet) == 0 {
		return 0, false
	}
	switch packet[0] >> 4 {
	case 4:
		return ipv4.ProtocolNumber, len(packet) >= header.IPv4MinimumSize
	case 6:
		return ipv6.ProtocolNumber, len(packet) >= header.IPv6MinimumSize
	default:
		return 0, false
	}
}

func (p *packetStack) validPlatformHeader(platform []byte, protocol tcpip.NetworkProtocolNumber) bool {
	if p.offset == 0 {
		return len(platform) == 0
	}
	if len(platform) != 4 {
		return false
	}
	family := binary.BigEndian.Uint32(platform)
	return family == unix.AF_INET && protocol == ipv4.ProtocolNumber ||
		family == unix.AF_INET6 && protocol == ipv6.ProtocolNumber
}

func (p *packetStack) acquire() bool {
	select {
	case p.admission <- struct{}{}:
		return true
	default:
		p.sessionRejected.Add(1)
		return false
	}
}

func (p *packetStack) release() { <-p.admission }

func (p *packetStack) forwardTCP(request *tcp.ForwarderRequest) {
	if !p.acquire() {
		request.Complete(true)
		return
	}
	defer p.release()
	id := request.ID()
	destination, err := endpointAddress(id.LocalAddress, id.LocalPort)
	if err != nil {
		request.Complete(true)
		return
	}
	outer, err := p.proxy.dialTCP(p.ctx, destination)
	if err != nil {
		request.Complete(true)
		p.log("warning", fmt.Sprintf("TCP proxy connection failed: %v", err))
		return
	}
	var queue waiter.Queue
	endpoint, tcpErr := request.CreateEndpoint(&queue)
	if tcpErr != nil {
		request.Complete(true)
		_ = outer.Close()
		return
	}
	request.Complete(false)
	inner := gonet.NewTCPConn(&queue, endpoint)
	bridgeTCP(p.ctx, inner, outer)
}

func (p *packetStack) forwardUDP(request *udp.ForwarderRequest) bool {
	if !p.acquire() {
		return false
	}
	go func() {
		defer p.release()
		id := request.ID()
		destination, err := endpointAddress(id.LocalAddress, id.LocalPort)
		if err != nil {
			return
		}
		var queue waiter.Queue
		endpoint, udpErr := request.CreateEndpoint(&queue)
		if udpErr != nil {
			return
		}
		inner := gonet.NewUDPConn(&queue, endpoint)
		defer inner.Close()
		outer, err := p.proxy.dialUDP(p.ctx)
		if err != nil {
			p.log("warning", fmt.Sprintf("UDP proxy association failed: %v", err))
			return
		}
		defer outer.Close()
		bridgeUDP(p.ctx, inner, outer, destination)
	}()
	return true
}

func endpointAddress(address tcpip.Address, port uint16) (netip.AddrPort, error) {
	raw := append([]byte(nil), (&address).AsSlice()...)
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.AddrPort{}, errors.New("invalid network-stack address")
	}
	return netip.AddrPortFrom(addr.Unmap(), port), nil
}

func bridgeTCP(parent context.Context, left *gonet.TCPConn, right net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer left.Close()
	defer right.Close()
	done := make(chan struct{}, 2)
	copySide := func(destination io.Writer, source io.Reader, closeDestination func() error, closeSource func() error) {
		_, _ = io.CopyBuffer(destination, source, make([]byte, copyBufferSize))
		_ = closeDestination()
		_ = closeSource()
		done <- struct{}{}
	}
	go copySide(right, left, func() error {
		if closer, ok := right.(interface{ CloseWrite() error }); ok {
			return closer.CloseWrite()
		}
		return right.Close()
	}, left.CloseRead)
	go copySide(left, right, left.CloseWrite, func() error {
		if closer, ok := right.(interface{ CloseRead() error }); ok {
			return closer.CloseRead()
		}
		return nil
	})
	select {
	case <-ctx.Done():
	case <-done:
	}
	_ = left.Close()
	_ = right.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func bridgeUDP(parent context.Context, inner *gonet.UDPConn, outer *socksUDPAssociation, destination netip.AddrPort) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{}, 2)
	refresh := func() {
		deadline := time.Now().Add(udpIdleTimeout)
		_ = inner.SetDeadline(deadline)
		_ = outer.SetDeadline(deadline)
	}
	refresh()
	go func() {
		defer func() { done <- struct{}{} }()
		buffer := make([]byte, udpPacketBufferSize)
		for {
			n, err := inner.Read(buffer)
			if err != nil || outer.WriteTo(buffer[:n], destination) != nil {
				return
			}
			refresh()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buffer := make([]byte, udpPacketBufferSize)
		for {
			n, err := outer.ReadFrom(buffer, destination)
			if err != nil || writeFull(inner, buffer[:n]) != nil {
				return
			}
			refresh()
		}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	_ = inner.Close()
	_ = outer.Close()
}
