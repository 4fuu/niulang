package main

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func parseRuntimeForTest(t *testing.T, client bool, args ...string) runtimeOptions {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts runtimeOptions
	bindRuntimeFlags(fs, &opts, client)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntime(opts, client); err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestClientDefaultsNeedOnlyAnImportedProfile(t *testing.T) {
	opts := parseRuntimeForTest(t, true)
	if opts.listen != "127.0.0.1:1080" || !opts.quicPool || opts.transport != "auto" {
		t.Fatalf("unexpected client defaults: %+v", opts)
	}
}

func TestServerDefaultsUseBothTransports(t *testing.T) {
	opts := parseRuntimeForTest(t, false)
	if opts.listen != ":443" || opts.transport != "auto" {
		t.Fatalf("unexpected server defaults: %+v", opts)
	}
}

func TestRuntimeBoundsRejectUnsafeValues(t *testing.T) {
	for _, args := range [][]string{
		{"--tcp-fallback-lanes", "17"},
		{"--max-sessions", "0"},
		{"--fallback-grace", "0s"},
		{"--flow-idle-timeout", "2h", "--flow-max-lifetime", "1h"},
		{"--local-address", "if:"},
	} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var opts runtimeOptions
		bindRuntimeFlags(fs, &opts, true)
		if err := fs.Parse(args); err != nil {
			continue
		}
		if err := validateRuntime(opts, true); err == nil {
			t.Fatalf("unsafe options accepted: %v", args)
		}
	}
}

func TestFallbackWindowsRemainConfigurable(t *testing.T) {
	opts := parseRuntimeForTest(t, true, "--fallback-delay", "25ms", "--fallback-grace", "3s")
	if opts.fallbackDelay != 25*time.Millisecond || opts.fallbackGrace != 3*time.Second {
		t.Fatalf("fallback windows = %v/%v", opts.fallbackDelay, opts.fallbackGrace)
	}
}

func TestLegacyModeInterfaceIsGone(t *testing.T) {
	if err := run([]string{"--mode", "local"}); err == nil {
		t.Fatal("legacy mode/secret interface was accepted")
	}
}

func TestEnrollmentURIAllowsFollowingFlags(t *testing.T) {
	err := runEnroll([]string{"queqiao://invalid", "--profile", "profile.json"})
	if err == nil || strings.Contains(err.Error(), "at most one") || strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("share URI prevented following flags from being parsed: %v", err)
	}
}

func TestClientMissingProfileExplainsEnrollment(t *testing.T) {
	err := runClient(nil)
	if err == nil || !strings.Contains(err.Error(), "queqiaod enroll") {
		t.Fatalf("missing profile produced unhelpful error: %v", err)
	}
}
