package session

import (
	"testing"
	"time"
)

func TestHelloRoundTripAndAuthentication(t *testing.T) {
	secret := []byte("a sufficiently long test secret")
	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	want, err := NewHello(secret, id, 4, HelloJoin, now)
	if err != nil {
		t.Fatal(err)
	}
	var got Hello
	if err := got.UnmarshalBinary(want.MarshalBinary()); err != nil {
		t.Fatal(err)
	}
	if err := got.Verify(secret, now); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := got.Verify([]byte("a different sufficiently long secret"), now); err == nil {
		t.Fatal("expected authentication failure")
	}
	if err := got.Verify(secret, now.Add(6*time.Minute)); err == nil {
		t.Fatal("expected clock-skew failure")
	}
}

func TestHelloOKRoundTrip(t *testing.T) {
	want, err := NewHelloOKWithCapabilities(time.Unix(1_700_000_000, 0), 0x1234)
	if err != nil {
		t.Fatal(err)
	}
	var got HelloOK
	if err := got.UnmarshalBinary(want.MarshalBinary()); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestHelloOKAcceptsLegacyCapabilityFreeEnvelope(t *testing.T) {
	want, err := NewHelloOK(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	legacy := want.MarshalBinary()
	if len(legacy) != helloOKSize {
		t.Fatalf("legacy acknowledgement length %d, want %d", len(legacy), helloOKSize)
	}
	var got HelloOK
	if err := got.UnmarshalBinary(legacy); err != nil {
		t.Fatalf("legacy acknowledgement rejected: %v", err)
	}
	if got.Timestamp != want.Timestamp || got.Nonce != want.Nonce || got.Capabilities != 0 {
		t.Fatalf("legacy acknowledgement decoded as %#v", got)
	}
}
