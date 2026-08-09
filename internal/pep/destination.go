package pep

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

type DestinationPolicy struct {
	AllowPrivate bool
	DialTimeout  time.Duration
}

var additionallyForbidden = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func (p DestinationPolicy) DialContext(ctx context.Context, destination string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(destination)
	if err != nil || host == "" {
		return nil, errors.New("invalid destination")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid destination port")
	}
	timeout := p.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var addresses []net.IPAddr
	if parsed := net.ParseIP(host); parsed != nil {
		addresses = []net.IPAddr{{IP: parsed}}
	} else {
		addresses, err = net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("destination resolution failed")
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("destination did not resolve")
	}

	dialer := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, candidate := range addresses {
		if !p.AllowPrivate && !publicDestinationIP(candidate.IP) {
			lastErr = errors.New("destination address is not public")
			continue
		}
		address := net.JoinHostPort(candidate.IP.String(), strconv.Itoa(port))
		conn, dialErr := dialer.DialContext(ctx, "tcp", address)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("destination unavailable")
	}
	return nil, fmt.Errorf("destination unavailable: %w", lastErr)
}

func publicDestinationIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range additionallyForbidden {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
