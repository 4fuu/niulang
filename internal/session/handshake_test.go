package session

import (
	"encoding/binary"
	"math"
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

func TestHelloOKAcceptsInterimExtendedEnvelope(t *testing.T) {
	want, err := NewHelloOK(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	legacy := want.MarshalBinary()
	extended := append(append([]byte(nil), legacy...), 0, 0, 0, 0, 0, 0, 0, 1)
	var got HelloOK
	if err := got.UnmarshalBinary(extended); err != nil {
		t.Fatalf("interim acknowledgement rejected: %v", err)
	}
	if got.Capabilities != CapabilityFastStreams {
		t.Fatalf("interim capability = %#x, want %#x", got.Capabilities, CapabilityFastStreams)
	}
}

func TestHandshakeRejectsTimestampsOutsideSignedUnixRange(t *testing.T) {
	secret := []byte("a sufficiently long test secret")
	now := time.Unix(1_700_000_000, 0)
	hello, err := NewHello(secret, [16]byte{}, 0, HelloNew, now)
	if err != nil {
		t.Fatal(err)
	}
	rawHello := hello.MarshalBinary()
	binary.BigEndian.PutUint64(rawHello[:8], math.MaxUint64)
	if err := new(Hello).UnmarshalBinary(rawHello); err == nil {
		t.Fatal("hello accepted a timestamp that would overflow int64")
	}

	ok, err := NewHelloOK(now)
	if err != nil {
		t.Fatal(err)
	}
	rawOK := ok.MarshalBinary()
	binary.BigEndian.PutUint64(rawOK[:8], math.MaxUint64)
	if err := new(HelloOK).UnmarshalBinary(rawOK); err == nil {
		t.Fatal("hello acknowledgement accepted a timestamp that would overflow int64")
	}
}

func TestTimestampSkewComparisonDoesNotOverflow(t *testing.T) {
	const skew = int64(maxClockSkew / time.Second)
	if timestampOutsideSkew(math.MaxInt64, math.MaxInt64, skew) {
		t.Fatal("equal upper-bound timestamps were rejected")
	}
	if timestampOutsideSkew(math.MinInt64, math.MinInt64, skew) {
		t.Fatal("equal lower-bound timestamps were rejected")
	}
	if !timestampOutsideSkew(math.MinInt64, math.MaxInt64, skew) {
		t.Fatal("maximally separated timestamps were accepted")
	}
}
