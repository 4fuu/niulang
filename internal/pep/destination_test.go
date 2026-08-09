package pep

import (
	"net"
	"testing"
)

func TestPublicDestinationIP(t *testing.T) {
	for _, test := range []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"100.64.0.1", false},
		{"169.254.169.254", false},
		{"192.0.2.1", false},
		{"2001:4860:4860::8888", true},
		{"::1", false},
		{"2001:db8::1", false},
	} {
		if got := publicDestinationIP(net.ParseIP(test.ip)); got != test.public {
			t.Errorf("publicDestinationIP(%s) = %v, want %v", test.ip, got, test.public)
		}
	}
}
