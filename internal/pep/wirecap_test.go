package pep

import (
	"testing"

	wancongestion "github.com/4fuu/niulang/internal/congestion"
)

func TestWireCapSetSharesOnlyOneEndpointPath(t *testing.T) {
	caps := newWireCapSet(1_000_000, 100_000)
	first := caps.scheduler("192.0.2.1")
	if got := caps.scheduler("192.0.2.1"); got != first {
		t.Fatal("connections on one provider path received separate schedulers")
	}
	if got := caps.scheduler("192.0.2.2"); got == first {
		t.Fatal("different endpoint paths shared one scheduler")
	}
	if newWireCapSet(0, 0) != nil {
		t.Fatal("the disabled default allocated a wire scheduler")
	}
}

type wireClassTelemetry struct{ bulk bool }

func (t *wireClassTelemetry) SetBulk(bulk bool) { t.bulk = bulk }
func (*wireClassTelemetry) Telemetry() wancongestion.ControllerTelemetry {
	return wancongestion.ControllerTelemetry{}
}

func TestWireClassChangesOnlyControllersThatSupportIt(t *testing.T) {
	controller := new(wireClassTelemetry)
	conn := &quicStreamConn{controller: controller}
	setWireBulk(conn, true)
	if !controller.bulk {
		t.Fatal("validated bulk connection did not change scheduler class")
	}
	setWireBulk(conn, false)
	if controller.bulk {
		t.Fatal("control replacement did not restore interactive scheduler class")
	}
	// Default and non-QUIC transports have no class setter and remain a no-op.
	setWireBulk(nil, true)
}
