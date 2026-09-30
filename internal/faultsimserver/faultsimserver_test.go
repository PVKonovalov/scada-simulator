package faultsimserver

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/faultsim"
	"scada-simulator/pkg/llog"
)

// startTestServer builds a Simulator named "test" with an instantaneous
// overcurrent stage armed at 1000A, serves it over a real TCP listener on
// an OS-assigned port, and returns a connected FaultSimulatorClient plus a
// cleanup func.
func startTestServer(t *testing.T) (faultsim.FaultSimulatorClient, *breaker.Simulator, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	sim := breaker.New(ctx, breaker.Config{
		Name:                  "test",
		InitialPosition:       breaker.PositionClosed,
		MechanicalOperateTime: time.Millisecond,
		SettingsGroups: []breaker.SettingsGroupConfig{{
			Group:    0,
			Settings: breaker.ProtectionSettings{InstantaneousPickup: 1000},
		}},
	})

	grpcServer := grpc.NewServer()
	faultsim.RegisterFaultSimulatorServer(grpcServer, NewFaultSimulatorServer(
		ctx,
		map[string]*breaker.Simulator{"test": sim},
		llog.Logger,
	))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(listener) }()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		grpcServer.Stop()
		sim.Close()
		cancel()
	}
	return faultsim.NewFaultSimulatorClient(conn), sim, cleanup
}

// TestInjectMeasurement_TripsBreaker checks that injecting a current above
// the breaker's instantaneous pickup actually trips it open, exactly like
// calling EmulatorFeed.InjectMeasurement directly.
func TestInjectMeasurement_TripsBreaker(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{
		Breaker: "test",
		Current: &faultsim.PhaseValues{A: 5000, B: 5000, C: 5000},
	})
	if err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	if resp.GetResult() != faultsim.InjectMeasurementResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pos, _ := sim.Position(); pos == breaker.PositionOpen {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("breaker did not trip open; ProtectionStatus = %+v", sim.ProtectionStatus())
}

// TestInjectMeasurement_StoresFullMeasurement checks that every field of a
// non-fault measurement reaches the breaker unchanged.
func TestInjectMeasurement_StoresFullMeasurement(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{
		Breaker:       "test",
		Current:       &faultsim.PhaseValues{A: 100, B: 110, C: 120},
		Voltage:       &faultsim.PhaseValues{A: 230, B: 231, C: 232},
		ActivePower:   1000,
		ReactivePower: 200,
		ApparentPower: 1020,
		Frequency:     50.1,
	})
	if err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	if resp.GetResult() != faultsim.InjectMeasurementResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	m := sim.Measurement()
	if m.Current != (breaker.PhaseValues{A: 100, B: 110, C: 120}) {
		t.Errorf("Current = %+v, want {100 110 120}", m.Current)
	}
	if m.Voltage != (breaker.PhaseValues{A: 230, B: 231, C: 232}) {
		t.Errorf("Voltage = %+v, want {230 231 232}", m.Voltage)
	}
	if m.ActivePower != 1000 || m.ReactivePower != 200 || m.ApparentPower != 1020 {
		t.Errorf("power = (%v, %v, %v), want (1000, 200, 1020)", m.ActivePower, m.ReactivePower, m.ApparentPower)
	}
	if m.Frequency != 50.1 {
		t.Errorf("Frequency = %v, want 50.1", m.Frequency)
	}
}

// startSustainTestServer is like startTestServer but with autoreclose
// enabled and dead/reclaim times comfortably larger than sustainInterval,
// so a sustained fault reliably lands at least one re-injection inside
// every DeadTime/Reclaim window.
func startSustainTestServer(t *testing.T) (faultsim.FaultSimulatorClient, *breaker.Simulator, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	sim := breaker.New(ctx, breaker.Config{
		Name:                  "test",
		InitialPosition:       breaker.PositionClosed,
		MechanicalOperateTime: time.Millisecond,
		SettingsGroups: []breaker.SettingsGroupConfig{{
			Group:    0,
			Settings: breaker.ProtectionSettings{InstantaneousPickup: 1000},
		}},
		AutoReclose: breaker.AutoRecloseConfig{
			Enabled:     true,
			MaxAttempts: 2,
			DeadTimes:   []time.Duration{500 * time.Millisecond, 500 * time.Millisecond},
			ReclaimTime: 500 * time.Millisecond,
		},
	})

	grpcServer := grpc.NewServer()
	faultsim.RegisterFaultSimulatorServer(grpcServer, NewFaultSimulatorServer(
		ctx,
		map[string]*breaker.Simulator{"test": sim},
		llog.Logger,
	))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(listener) }()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		grpcServer.Stop()
		sim.Close()
		cancel()
	}
	return faultsim.NewFaultSimulatorClient(conn), sim, cleanup
}

// TestInjectMeasurement_SustainsUntilLockout checks that a single
// InjectMeasurement call is not a one-shot event: without any further
// calls, the fault keeps being re-fed to the breaker (see sustain), so it
// survives every autoreclose attempt and reaches Lockout on its own.
func TestInjectMeasurement_SustainsUntilLockout(t *testing.T) {
	client, sim, cleanup := startSustainTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{
		Breaker: "test",
		Current: &faultsim.PhaseValues{A: 5000, B: 5000, C: 5000},
	})
	if err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	if resp.GetResult() != faultsim.InjectMeasurementResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if sim.AutoRecloseStatus().State == breaker.AutoRecloseLockout {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("AutoRecloseStatus().State = %v, want Lockout (fault should have been sustained across every reclose attempt)", sim.AutoRecloseStatus().State)
}

// TestInjectMeasurement_ClearedBySubsequentLowerInjection checks that a
// sustained fault stops being re-injected as soon as a later
// InjectMeasurement call for the same breaker replaces it with a value
// below pickup — the breaker then recloses successfully instead of being
// re-tripped forever.
func TestInjectMeasurement_ClearedBySubsequentLowerInjection(t *testing.T) {
	client, sim, cleanup := startSustainTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{
		Breaker: "test",
		Current: &faultsim.PhaseValues{A: 5000, B: 5000, C: 5000},
	})
	if err != nil {
		t.Fatalf("InjectMeasurement (fault): %v", err)
	}
	if resp.GetResult() != faultsim.InjectMeasurementResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	// Wait for the first trip, then clear the fault well within the first
	// DeadTime — the sustain goroutine must stop re-injecting 5000A from
	// this point on.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sim.ProtectionStatus().State != breaker.ProtectionTripped {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sim.ProtectionStatus().State; got != breaker.ProtectionTripped {
		t.Fatalf("ProtectionStatus().State = %v, want Tripped before clearing", got)
	}

	resp, err = client.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{
		Breaker: "test",
		Current: &faultsim.PhaseValues{A: 0, B: 0, C: 0},
	})
	if err != nil {
		t.Fatalf("InjectMeasurement (clear): %v", err)
	}
	if resp.GetResult() != faultsim.InjectMeasurementResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	// The reclose sequence should now succeed and settle back to Ready
	// (not advance to a second failed attempt, and never reach Lockout).
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sim.AutoRecloseStatus().State == breaker.AutoRecloseLockout {
			t.Fatalf("AutoRecloseStatus().State reached Lockout; clearing the fault should have let it reclose")
		}
		if sim.AutoRecloseStatus().State == breaker.AutoRecloseReady && sim.ProtectionStatus().State == breaker.ProtectionNormal {
			if pos, _ := sim.Position(); pos == breaker.PositionClosed {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not settle back to Ready/Normal/Closed after clearing; AutoRecloseStatus=%+v ProtectionStatus=%+v",
		sim.AutoRecloseStatus(), sim.ProtectionStatus())
}

// TestInjectMeasurement_UnknownBreaker checks that an unrecognised breaker
// name is rejected without affecting the configured breaker's state.
func TestInjectMeasurement_UnknownBreaker(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	before := sim.Measurement()

	resp, err := client.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{
		Breaker: "no-such-breaker",
		Current: &faultsim.PhaseValues{A: 5000, B: 5000, C: 5000},
	})
	if err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	if resp.GetResult() != faultsim.InjectMeasurementResult_ERROR_BREAKER_NOT_FOUND {
		t.Errorf("result = %v, want ERROR_BREAKER_NOT_FOUND", resp.GetResult())
	}

	if got := sim.Measurement(); got != before {
		t.Errorf("Measurement() = %+v, want unchanged %+v", got, before)
	}
}

// TestInjectMeasurement_Handoff checks that a zero-current injection
// releases (rather than sustains) a breaker only when a handoff feed
// handles it, and that any other injection keeps Overriding true.
func TestInjectMeasurement_Handoff(t *testing.T) {
	tests := []struct {
		name         string
		handoff      func(string) bool
		current      float64
		wantOverride bool
		wantCurrentA float64
	}{
		{"no handoff, zero current sustains", nil, 0, true, 0},
		{"handoff declines, zero current sustains", func(string) bool { return false }, 0, true, 0},
		{"handoff, zero current releases", func(string) bool { return true }, 0, false, 0},
		{"handoff, nonzero current sustains", func(string) bool { return true }, 100, true, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sim := breaker.New(ctx, breaker.Config{Name: "test", InitialPosition: breaker.PositionClosed})
			defer sim.Close()
			s := NewFaultSimulatorServer(ctx, map[string]*breaker.Simulator{"test": sim}, llog.Logger).WithHandoff(tt.handoff)

			// A prior fault must be superseded either way.
			if _, err := s.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{Breaker: "test", Current: &faultsim.PhaseValues{A: 50}}); err != nil {
				t.Fatalf("InjectMeasurement: %v", err)
			}
			if !s.Overriding("test") {
				t.Fatalf("Overriding after first injection = false, want true")
			}

			req := &faultsim.InjectMeasurementRequest{Breaker: "test", Current: &faultsim.PhaseValues{A: tt.current, B: tt.current, C: tt.current}, Voltage: &faultsim.PhaseValues{A: 230}}
			if _, err := s.InjectMeasurement(ctx, req); err != nil {
				t.Fatalf("InjectMeasurement: %v", err)
			}
			if got := s.Overriding("test"); got != tt.wantOverride {
				t.Errorf("Overriding = %v, want %v", got, tt.wantOverride)
			}
			time.Sleep(2 * sustainInterval) // a cancelled sustain goroutine must not re-feed the old value
			if m := sim.Measurement(); m.Current.A != tt.wantCurrentA || m.Voltage.A != 230 {
				t.Errorf("measurement = %+v, want current.a %v and voltage.a 230", m, tt.wantCurrentA)
			}
		})
	}
}
