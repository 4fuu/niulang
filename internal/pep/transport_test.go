package pep

import (
	"strings"
	"testing"
)

func TestValidateLocalAddressSpec(t *testing.T) {
	for _, spec := range []string{"", "auto", "if:en0", "192.0.2.10", "2001:db8::10"} {
		if err := validateLocalAddressSpec(spec); err != nil {
			t.Errorf("validateLocalAddressSpec(%q): %v", spec, err)
		}
	}
	for _, spec := range []string{"not-an-address", "if:", "if:   "} {
		if err := validateLocalAddressSpec(spec); err == nil {
			t.Errorf("validateLocalAddressSpec(%q) unexpectedly succeeded", spec)
		}
	}
}

func TestResolveLocalAddressLiteral(t *testing.T) {
	got, err := resolveLocalAddress("192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "192.0.2.10" {
		t.Fatalf("resolved literal = %s", got)
	}
}

func TestResolveLocalAddressAutoOrInterfaceReportsOperationalState(t *testing.T) {
	// The CI host may have no physical IPv4 interface, or may expose more than
	// one. In either case the important contract is a bounded, actionable error;
	// when auto succeeds it must return an IPv4 address that can be bound.
	got, err := resolveLocalAddress("auto")
	if err != nil {
		if !strings.Contains(err.Error(), "IPv4") && !strings.Contains(err.Error(), "physical") {
			t.Fatalf("unexpected auto-resolution error: %v", err)
		}
		return
	}
	if !got.Is4() {
		t.Fatalf("auto selected non-IPv4 address %s", got)
	}
}
