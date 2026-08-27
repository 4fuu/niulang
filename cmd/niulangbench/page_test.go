package main

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

func TestStorefrontPageProfileKeepsVisibleResourcesAheadOfMediaTail(t *testing.T) {
	if got, want := storefrontPageProfile.criticalBytes(), int64(4000<<10); got != want {
		t.Fatalf("critical bytes = %d, want %d", got, want)
	}
	if got, want := storefrontPageProfile.totalBytes(), int64(12192<<10); got != want {
		t.Fatalf("total bytes = %d, want %d", got, want)
	}
	seen := make(map[string]bool, len(storefrontPageProfile.resources))
	var first, tail *pageResourceSpec
	for i := range storefrontPageProfile.resources {
		resource := &storefrontPageProfile.resources[i]
		if seen[resource.name] {
			t.Fatalf("duplicate resource %q in %s", resource.name, storefrontPageProfile.name)
		}
		seen[resource.name] = true
		switch resource.name {
		case videoFirstResource:
			first = resource
		case "video-tail":
			tail = resource
		}
	}
	if first == nil || !first.critical || first.bytes != 512<<10 {
		t.Fatalf("video first segment = %+v, want a critical 512 KiB prefix", first)
	}
	if tail == nil || tail.critical || tail.startsAfter != videoFirstResource {
		t.Fatalf("video tail = %+v, want background media after the first segment", tail)
	}
}

func TestPageOriginReturnsTheRequestedResourceSize(t *testing.T) {
	origin, err := newOrigin(1)
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	conn, err := net.Dial("tcp", origin.pageAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const size = 12345
	var request [8]byte
	binary.BigEndian.PutUint64(request[:], size)
	if _, err := conn.Write(request[:]); err != nil {
		t.Fatal(err)
	}
	if n, err := io.CopyN(io.Discard, conn, size); err != nil || n != size {
		t.Fatalf("resource = %d bytes, %v; want %d bytes", n, err, size)
	}
}

func TestSummarizePagesKeepsCriticalAndVideoTailMetricsSeparate(t *testing.T) {
	var reports []PageReport
	for i := 1; i <= 5; i++ {
		reports = append(reports, PageReport{
			CriticalCompleteMillis:  float64(i * 100),
			CriticalSpreadMillis:    float64(i * 10),
			VideoFirstSegmentMillis: float64(i * 50),
			FullCompleteMillis:      float64(i * 1000),
		})
	}
	got := summarizePages(reports)
	if got == nil || got.Samples != 5 || got.CriticalCompleteP50Millis != 300 || got.CriticalCompleteP95Millis != 400 ||
		got.VideoFirstSegmentP50Millis != 150 || got.VideoFirstSegmentP95Millis != 200 ||
		got.FullCompleteP50Millis != 3000 || got.FullCompleteP95Millis != 4000 {
		t.Fatalf("page summary = %+v", got)
	}
}

func TestPageOptionsRequireOneStandalonePage(t *testing.T) {
	for _, args := range [][]string{
		{"--page", "--flows", "2"},
		{"--page", "--interactive"},
		{"--page", "--contend", "niulang,baseline"},
	} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "--page") {
			t.Fatalf("run(%v) error = %v, want page validation", args, err)
		}
	}
}
