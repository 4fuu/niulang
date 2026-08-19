package mobilecore

import (
	"encoding/json"
	"testing"
)

func TestMobileResourceProfilesHaveFixedEndpointBudgets(t *testing.T) {
	for _, limits := range []mobileResourceLimits{iosResourceLimits, androidResourceLimits} {
		t.Run(limits.name, func(t *testing.T) {
			if limits.maxSessions <= 0 || limits.maxPendingOpens <= 0 || limits.maxPendingOpens > limits.maxSessions {
				t.Fatalf("invalid admission limits: %+v", limits)
			}
			if limits.memory.SendBudgetBytes <= 0 || limits.memory.ReceiveBudgetBytes <= 0 {
				t.Fatal("shared payload budgets are disabled")
			}
			if limits.memory.MaxFlowSendBytes > int(limits.memory.SendBudgetBytes) ||
				limits.memory.MaxFlowReceiveBytes > uint64(limits.memory.ReceiveBudgetBytes) {
				t.Fatal("one flow exceeds its endpoint-wide budget")
			}
			if limits.streamWindow != limits.connectionWindow && limits.streamWindow > limits.connectionWindow {
				t.Fatal("stream receive window exceeds connection window")
			}
		})
	}
}

func TestMetricsExposeTheSelectedMemoryEnvelope(t *testing.T) {
	session := &Session{state: StateStopped, resources: iosResourceLimits}
	var got struct {
		Version int `json:"version"`
		Memory  struct {
			Profile string `json:"profile"`
			GoLimit int64  `json:"go_limit_bytes"`
		} `json:"memory"`
	}
	if err := json.Unmarshal([]byte(session.MetricsJSON()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Memory.Profile != iosResourceLimits.name || got.Memory.GoLimit != iosResourceLimits.goMemoryLimit {
		t.Fatalf("metrics memory envelope = %+v", got)
	}
}
