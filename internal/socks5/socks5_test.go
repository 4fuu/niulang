package socks5

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func requestBytes(atyp byte, address []byte, port uint16) []byte {
	b := []byte{5, 1, 0, 5, 1, 0, atyp}
	b = append(b, address...)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], port)
	return append(b, p[:]...)
}

func TestReadDomainConnect(t *testing.T) {
	b := requestBytes(3, append([]byte{11}, []byte("example.com")...), 443)
	rw := bytes.NewBuffer(b)
	req, err := ReadRequest(rw)
	if err != nil {
		t.Fatal(err)
	}
	if req.Destination != "example.com:443" {
		t.Fatalf("destination %q", req.Destination)
	}
	reply := rw.Bytes()
	if len(reply) != 2 || reply[0] != 5 || reply[1] != 0 {
		t.Fatalf("method reply %v", reply)
	}
}

func TestRejectUnsupportedCommand(t *testing.T) {
	b := []byte{5, 1, 0, 5, 3, 0, 1, 127, 0, 0, 1, 0, 53}
	rw := bytes.NewBuffer(b)
	if _, err := ReadRequest(rw); err == nil {
		t.Fatal("expected rejection")
	}
	got := rw.Bytes()
	if len(got) < 10 || got[len(got)-9] != ReplyCommandNotSupported {
		t.Fatalf("unexpected reply %v", got)
	}
}
