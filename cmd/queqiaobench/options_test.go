package main

import (
	"strings"
	"testing"
)

func TestPolicerOptionsRejectIncompleteConfigurations(t *testing.T) {
	for name, args := range map[string][]string{
		"refill without rate":  {"--policer-refill", "8ms", "--rate", "0"},
		"burst without refill": {"--policer-burst", "4000"},
		"negative refill":      {"--policer-refill", "-1ms"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil || !strings.Contains(err.Error(), "policer") {
				t.Fatalf("run(%v) error = %v, want policer validation", args, err)
			}
		})
	}
}

func TestExperimentBudgetsRejectIncompleteConfigurations(t *testing.T) {
	for name, args := range map[string][]string{
		"uncompensated brutal without rate": {"--congestion", "brutal-no-comp"},
		"reserve without aggregate":         {"--interactive-reserve", "1"},
		"reserve consumes aggregate":        {"--aggregate-rate", "1", "--interactive-reserve", "1"},
		"wire reserve without cap":          {"--wire-interactive-reserve", "1"},
		"wire reserve consumes cap":         {"--wire-cap-rate", "1", "--wire-interactive-reserve", "1"},
		"wire cap cannot wrap Reno":         {"--wire-cap-rate", "1", "--congestion", "reno"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil {
				t.Fatalf("run(%v) accepted an incomplete experiment", args)
			}
		})
	}
}

func TestUDPOptionsRejectInvalidWorkloads(t *testing.T) {
	for name, args := range map[string][]string{
		"negative packets":   {"--udp-packets", "-1"},
		"short sequence":     {"--udp-packets", "1", "--udp-payload", "3"},
		"negative interval":  {"--udp-packets", "1", "--udp-interval", "-1ms"},
		"no settle interval": {"--udp-packets", "1", "--udp-settle", "0s"},
		"shared contention":  {"--udp-packets", "1", "--contend", "queqiao,baseline"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil || !strings.Contains(strings.ToLower(err.Error()), "udp") {
				t.Fatalf("run(%v) error = %v, want UDP validation", args, err)
			}
		})
	}
}
