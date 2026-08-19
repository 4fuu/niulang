package mobilecore

import (
	"runtime/debug"

	"github.com/bojieli/queqiao/internal/pep"
)

// mobileResourceLimits is the fixed hardware-style capacity plan for one
// tunnel process. The dominant retransmit and reassembly arenas are shared
// between every flow. The remaining per-flow buffers are deliberately tiny
// and their aggregate is bounded by maxSessions, so every term in the memory
// envelope has a fixed endpoint-wide ceiling.
type mobileResourceLimits struct {
	name               string
	goMemoryLimit      int64
	maxSessions        int
	maxPendingOpens    int
	maxPayload         uint32
	chunkSize          int
	streamWindow       uint64
	connectionWindow   uint64
	maxIncomingStreams int64
	memory             pep.MemoryLimits
}

var iosResourceLimits = mobileResourceLimits{
	name: "ios-fixed-40m", goMemoryLimit: 40 * 1024 * 1024,
	maxSessions: 64, maxPendingOpens: 16, maxPayload: 64 * 1024, chunkSize: 16 * 1024,
	streamWindow: 512 * 1024, connectionWindow: 2 * 1024 * 1024, maxIncomingStreams: 32,
	memory: pep.MemoryLimits{
		SendBudgetBytes: 3 * 1024 * 1024, ReceiveBudgetBytes: 3 * 1024 * 1024,
		MaxFlowSendBytes: 1024 * 1024, MaxFlowReceiveBytes: 1024 * 1024,
		MaxFlowOutstanding: 64, MaxFlowReceiveFrames: 128,
		EventQueueFrames: 2, LaneWriteQueueFrames: 4, LaneInteractiveReserve: 1,
		FrameReadBufferBytes: 8 * 1024, MaxUDPPacketBytes: 4 * 1024, MaxBulkConnections: 1,
	},
}

var androidResourceLimits = mobileResourceLimits{
	name: "android-fixed-72m", goMemoryLimit: 72 * 1024 * 1024,
	maxSessions: 128, maxPendingOpens: 32, maxPayload: 64 * 1024, chunkSize: 16 * 1024,
	streamWindow: 1024 * 1024, connectionWindow: 4 * 1024 * 1024, maxIncomingStreams: 64,
	memory: pep.MemoryLimits{
		SendBudgetBytes: 8 * 1024 * 1024, ReceiveBudgetBytes: 8 * 1024 * 1024,
		MaxFlowSendBytes: 2 * 1024 * 1024, MaxFlowReceiveBytes: 2 * 1024 * 1024,
		MaxFlowOutstanding: 128, MaxFlowReceiveFrames: 256,
		EventQueueFrames: 4, LaneWriteQueueFrames: 4, LaneInteractiveReserve: 1,
		FrameReadBufferBytes: 8 * 1024, MaxUDPPacketBytes: 4 * 1024, MaxBulkConnections: 2,
	},
}

func applyRuntimeLimits(limits mobileResourceLimits) {
	// SetMemoryLimit is a safety backstop around all Go-owned memory, including
	// gVisor and quic-go. Exact payload admission is enforced by the budgets;
	// this soft runtime limit catches allocations outside those audited paths.
	debug.SetMemoryLimit(limits.goMemoryLimit)
	debug.SetGCPercent(50)
}
