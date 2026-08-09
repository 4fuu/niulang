package session

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const maxDestinationLength = 255

// EncodeDestination stores a single TCP destination in a bounded, canonical
// host:port representation. DNS resolution deliberately happens on the US
// agent so that the destination's egress remains there.
func EncodeDestination(destination string) ([]byte, error) {
	destination = strings.TrimSpace(destination)
	if len(destination) == 0 || len(destination) > maxDestinationLength {
		return nil, errors.New("destination is empty or too long")
	}
	host, port, err := net.SplitHostPort(destination)
	if err != nil || host == "" {
		return nil, errors.New("destination must be host:port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, errors.New("destination port is invalid")
	}
	// Re-encode to avoid ambiguous forms and to preserve IPv6 brackets.
	canonical := net.JoinHostPort(host, strconv.Itoa(p))
	return []byte(canonical), nil
}

func DecodeDestination(payload []byte) (string, error) {
	if len(payload) == 0 || len(payload) > maxDestinationLength {
		return "", errors.New("invalid destination payload length")
	}
	destination := string(payload)
	if _, err := EncodeDestination(destination); err != nil {
		return "", fmt.Errorf("invalid destination: %w", err)
	}
	return destination, nil
}
