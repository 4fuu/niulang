package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"time"

	"github.com/4fuu/niulang/internal/pathmodel"
	"github.com/4fuu/niulang/internal/pathsim"
)

const videoFirstResource = "video-first-segment"

type pageResourceSpec struct {
	name        string
	bytes       int64
	critical    bool
	startsAfter string
}

type pageProfile struct {
	name      string
	resources []pageResourceSpec
}

// storefront-v1 represents one navigation after the browser has discovered
// its subresources. Every visible resource and the first media segment starts
// together. The remaining media starts only after that first segment arrives,
// matching a player that can begin playback before downloading its tail.
//
// These are fixed benchmark units, not claims about Steam's current asset
// sizes. A fixed profile is what makes changes comparable across commits.
var storefrontPageProfile = pageProfile{
	name: "storefront-v1",
	resources: []pageResourceSpec{
		{name: "document", bytes: 64 << 10, critical: true},
		{name: "styles", bytes: 96 << 10, critical: true},
		{name: "runtime-js", bytes: 192 << 10, critical: true},
		{name: "application-js", bytes: 512 << 10, critical: true},
		{name: "icon-atlas", bytes: 64 << 10, critical: true},
		{name: "thumbnails", bytes: 256 << 10, critical: true},
		{name: "screenshot", bytes: 768 << 10, critical: true},
		{name: "hero-image", bytes: 1536 << 10, critical: true},
		{name: videoFirstResource, bytes: 512 << 10, critical: true},
		{name: "video-tail", bytes: 8 << 20, startsAfter: videoFirstResource},
	},
}

func (p pageProfile) totalBytes() int64 {
	var total int64
	for _, resource := range p.resources {
		total += resource.bytes
	}
	return total
}

func (p pageProfile) criticalBytes() int64 {
	var total int64
	for _, resource := range p.resources {
		if resource.critical {
			total += resource.bytes
		}
	}
	return total
}

func measurePage(stack string, opts options, pathCfg pathsim.Config, origin *origin, trial int) trialResult {
	pathmodel.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	cfg := pathCfg
	cfg.Seed = pathCfg.Seed + int64(trial)*1000
	harness, err := startStack(ctx, stack, opts, cfg)
	if err != nil {
		return trialResult{note: "setup: " + err.Error()}
	}
	defer harness.Close()
	if err := warmUp(ctx, harness.socks, origin); err != nil {
		up, down := harness.relay.Stats()
		pathCounters := describePathCounters(up, down)
		return trialResult{note: "warmup: " + err.Error(), pathCounters: &pathCounters}
	}

	page, received, pageErr := runPage(ctx, harness.socks, origin.pageAddr, storefrontPageProfile)
	settle := time.NewTimer(time.Duration(opts.rttMillis)*time.Millisecond + 20*time.Millisecond)
	select {
	case <-settle.C:
	case <-ctx.Done():
		if !settle.Stop() {
			<-settle.C
		}
	}
	if harness.clientMetrics != nil {
		page.BulkIsolations = harness.clientMetrics.Snapshot().BulkIsolations
	}

	up, down := harness.relay.Stats()
	pathCounters := describePathCounters(up, down)
	note := fmt.Sprintf("page_critical=%.0fms video_first=%.0fms page_full=%.0fms bulk_isolations=%d up=%d/%d,lost=%d,bottleneck_drop=%d down=%d/%d,lost=%d,bottleneck_drop=%d",
		page.CriticalCompleteMillis, page.VideoFirstSegmentMillis, page.FullCompleteMillis, page.BulkIsolations,
		up.PacketsOut, up.PacketsIn, up.PacketsLost, up.PacketsDropped,
		down.PacketsOut, down.PacketsIn, down.PacketsLost, down.PacketsDropped)
	if pageErr != nil {
		note = "page: " + pageErr.Error() + " " + note
	}
	seconds := page.FullCompleteMillis / 1000
	mbits := 0.0
	if seconds > 0 {
		mbits = float64(received) * 8 / seconds / 1e6
	}
	return trialResult{
		seconds: seconds, mbitsPerSec: mbits, complete: pageErr == nil, note: note,
		page: &page, bulkIsolations: page.BulkIsolations,
		pathCounters: &pathCounters, wireCap: harness.wireCapReport(),
	}
}

type pageResourceResult struct {
	spec   pageResourceSpec
	report PageResourceReport
	err    error
}

func runPage(ctx context.Context, socksAddr, destination string, profile pageProfile) (PageReport, int64, error) {
	started := time.Now()
	start := make(chan struct{})
	results := make(chan pageResourceResult, len(profile.resources))
	videoDone := make(chan pageResourceResult, 1)
	var dependent *pageResourceSpec

	for i := range profile.resources {
		spec := profile.resources[i]
		if spec.startsAfter != "" {
			copy := spec
			dependent = &copy
			continue
		}
		go func() {
			<-start
			result := fetchPageResource(ctx, socksAddr, destination, started, spec)
			results <- result
			if spec.name == videoFirstResource {
				videoDone <- result
			}
		}()
	}
	if dependent != nil {
		spec := *dependent
		go func() {
			first := <-videoDone
			if first.err != nil {
				now := round3(float64(time.Since(started).Microseconds()) / 1000)
				results <- pageResourceResult{
					spec: spec,
					report: PageResourceReport{Name: spec.name, Bytes: spec.bytes, StartedMillis: now, CompleteMillis: now,
						Note: "dependency failed: " + first.err.Error()},
					err: errors.New("video tail dependency failed"),
				}
				return
			}
			results <- fetchPageResource(ctx, socksAddr, destination, started, spec)
		}()
	}
	close(start)

	byName := make(map[string]pageResourceResult, len(profile.resources))
	var errs []error
	var received int64
	for range profile.resources {
		result := <-results
		byName[result.spec.name] = result
		received += result.report.Received
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.spec.name, result.err))
		}
	}

	report := PageReport{
		Profile: profile.name, CriticalBytes: profile.criticalBytes(), TotalBytes: profile.totalBytes(),
		Resources: make([]PageResourceReport, 0, len(profile.resources)),
	}
	criticalEnds := make([]float64, 0, len(profile.resources))
	for _, spec := range profile.resources {
		result := byName[spec.name]
		report.Resources = append(report.Resources, result.report)
		if spec.critical {
			criticalEnds = append(criticalEnds, result.report.CompleteMillis)
			if result.report.CompleteMillis > report.CriticalCompleteMillis {
				report.CriticalCompleteMillis = result.report.CompleteMillis
			}
		}
		if spec.name == videoFirstResource {
			report.VideoFirstSegmentMillis = result.report.CompleteMillis
		}
		if result.report.CompleteMillis > report.FullCompleteMillis {
			report.FullCompleteMillis = result.report.CompleteMillis
		}
	}
	if len(criticalEnds) > 0 {
		sort.Float64s(criticalEnds)
		report.CriticalSpreadMillis = round3(criticalEnds[len(criticalEnds)-1] - criticalEnds[0])
	}
	return report, received, errors.Join(errs...)
}

func fetchPageResource(ctx context.Context, socksAddr, destination string, pageStarted time.Time, spec pageResourceSpec) pageResourceResult {
	started := time.Now()
	var request [8]byte
	binary.BigEndian.PutUint64(request[:], uint64(spec.bytes))
	received, stages, err := fetchTimedRequest(ctx, socksAddr, destination, spec.bytes, request[:])
	report := PageResourceReport{
		Name: spec.name, Bytes: spec.bytes, Received: received, Critical: spec.critical,
		StartedMillis:   round3(float64(started.Sub(pageStarted).Microseconds()) / 1000),
		ConnectMillis:   round3(float64(stages.Connect.Microseconds()) / 1000),
		FirstByteMillis: round3(float64(stages.FirstByte.Microseconds()) / 1000),
		CompleteMillis:  round3(float64(time.Since(pageStarted).Microseconds()) / 1000),
		Complete:        err == nil && received == spec.bytes,
	}
	if err != nil {
		report.Note = err.Error()
	}
	return pageResourceResult{spec: spec, report: report, err: err}
}

const maxPageResourceBytes = 64 << 20

func (o *origin) servePage() {
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	for {
		conn, err := o.pageListener.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			var request [8]byte
			if _, err := io.ReadFull(conn, request[:]); err != nil {
				return
			}
			remaining := int64(binary.BigEndian.Uint64(request[:]))
			if remaining <= 0 || remaining > maxPageResourceBytes {
				return
			}
			for remaining > 0 {
				chunk := int64(len(payload))
				if chunk > remaining {
					chunk = remaining
				}
				n, err := conn.Write(payload[:chunk])
				if err != nil {
					return
				}
				remaining -= int64(n)
			}
		}(conn)
	}
}
