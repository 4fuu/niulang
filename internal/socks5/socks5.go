// Package socks5 implements the deliberately small SOCKS5 surface used by
// the local agent. It supports no-authentication TCP CONNECT requests and
// rejects BIND and UDP ASSOCIATE explicitly.
package socks5

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const (
	version5             = 5
	methodNone           = 0
	methodNoneAcceptable = 0xff

	CommandConnect = 1

	ReplySucceeded              = 0
	ReplyGeneralFailure         = 1
	ReplyConnectionNotAllowed   = 2
	ReplyNetworkUnreachable     = 3
	ReplyHostUnreachable        = 4
	ReplyConnectionRefused      = 5
	ReplyTTLExpired             = 6
	ReplyCommandNotSupported    = 7
	ReplyAddressTypeUnsupported = 8
)

type Request struct {
	Command     byte
	Destination string
}

func ReadRequest(rw io.ReadWriter) (Request, error) {
	var greeting [2]byte
	if _, err := io.ReadFull(rw, greeting[:]); err != nil {
		return Request{}, fmt.Errorf("read greeting: %w", err)
	}
	if greeting[0] != version5 || greeting[1] == 0 {
		return Request{}, errors.New("invalid SOCKS5 greeting")
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(rw, methods); err != nil {
		return Request{}, fmt.Errorf("read methods: %w", err)
	}
	found := false
	for _, method := range methods {
		if method == methodNone {
			found = true
			break
		}
	}
	if !found {
		_, _ = rw.Write([]byte{version5, methodNoneAcceptable})
		return Request{}, errors.New("client did not offer no-authentication method")
	}
	if err := writeFull(rw, []byte{version5, methodNone}); err != nil {
		return Request{}, fmt.Errorf("write method selection: %w", err)
	}

	var header [4]byte
	if _, err := io.ReadFull(rw, header[:]); err != nil {
		return Request{}, fmt.Errorf("read request header: %w", err)
	}
	if header[0] != version5 || header[2] != 0 {
		return Request{}, errors.New("invalid SOCKS5 request header")
	}
	if header[1] != CommandConnect {
		_ = WriteReply(rw, ReplyCommandNotSupported, nil)
		return Request{}, errors.New("only SOCKS5 CONNECT is supported")
	}
	host, err := readHost(rw, header[3])
	if err != nil {
		_ = WriteReply(rw, ReplyAddressTypeUnsupported, nil)
		return Request{}, err
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(rw, portBytes[:]); err != nil {
		return Request{}, fmt.Errorf("read destination port: %w", err)
	}
	port := binary.BigEndian.Uint16(portBytes[:])
	if port == 0 {
		_ = WriteReply(rw, ReplyAddressTypeUnsupported, nil)
		return Request{}, errors.New("destination port must not be zero")
	}
	return Request{Command: header[1], Destination: net.JoinHostPort(host, strconv.Itoa(int(port)))}, nil
}

func readHost(r io.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		var ip [4]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", fmt.Errorf("read IPv4 address: %w", err)
		}
		return net.IP(ip[:]).String(), nil
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return "", fmt.Errorf("read domain length: %w", err)
		}
		if length[0] == 0 {
			return "", errors.New("empty domain name")
		}
		domain := make([]byte, int(length[0]))
		if _, err := io.ReadFull(r, domain); err != nil {
			return "", fmt.Errorf("read domain name: %w", err)
		}
		return string(domain), nil
	case 4:
		var ip [16]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", fmt.Errorf("read IPv6 address: %w", err)
		}
		return net.IP(ip[:]).String(), nil
	default:
		return "", fmt.Errorf("unsupported SOCKS5 address type %d", addressType)
	}
}

func WriteReply(w io.Writer, code byte, bound net.Addr) error {
	ip := net.IPv4zero
	port := 0
	if tcp, ok := bound.(*net.TCPAddr); ok && tcp != nil {
		ip = tcp.IP
		port = tcp.Port
	}
	var response []byte
	if ip4 := ip.To4(); ip4 != nil {
		response = make([]byte, 10)
		response[0], response[1], response[2], response[3] = version5, code, 0, 1
		copy(response[4:8], ip4)
		binary.BigEndian.PutUint16(response[8:10], uint16(port))
	} else {
		response = make([]byte, 22)
		response[0], response[1], response[2], response[3] = version5, code, 0, 4
		copy(response[4:20], ip.To16())
		binary.BigEndian.PutUint16(response[20:22], uint16(port))
	}
	return writeFull(w, response)
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
