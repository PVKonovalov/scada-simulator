package telemetryserver

import (
	"context"
	"testing"
	"time"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/telemetry"
)

// TestSupervisoryControl_SelectThenExecute checks the two-step
// select-before-operate flow: Select arms the command, and a later Execute
// for the same key carries it out.
func TestSupervisoryControl_SelectThenExecute(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sel, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Select: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("select result = %v, want OK", sel.GetResult())
	}

	exec, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Execute: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if exec.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("execute result = %v, want OK", exec.GetResult())
	}

	waitForPosition(t, sim, breaker.PositionOpen)
}

// TestSupervisoryControl_SelectAndExecuteTogether checks that a single
// request with both Select and Execute set performs the command in one
// call, without a separate select phase.
func TestSupervisoryControl_SelectAndExecuteTogether(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Select: true, Execute: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("select+execute: %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	waitForPosition(t, sim, breaker.PositionOpen)
}

// TestSupervisoryControl_Test checks that Test only changes the quality
// flag — it does not by itself move the breaker or change what Select/
// Execute would otherwise do; combined with Select+Execute it performs a
// real Select+Execute (the breaker actually moves) and marks the breaker's
// tags qds.QdsTest — visible on Subscribe as DataPointQuality_QDS_TEST —
// until a later call explicitly sets Test back to false, which clears it.
func TestSupervisoryControl_Test(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, &telemetry.SubstationRequest{Id: "unused"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := stream.Recv(); err != nil { // discard initial snapshot
		t.Fatalf("Recv (snapshot): %v", err)
	}

	resp, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Select: true, Execute: true, Test: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	// Select+Execute do the real work; Test only adds the quality marking —
	// the breaker actually moves, and every pushed tag carries QDS_TEST
	// quality along the way.
	for {
		update, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		points := dataPointMap(t, update)
		for key, p := range points {
			if p.GetQuality() != telemetry.DataPointQuality_QDS_TEST {
				t.Errorf("%s quality = %v during test mode, want QDS_TEST", key, p.GetQuality())
			}
		}
		if pos, ok := points["test.position"]; ok && pos.GetValue() == float32(breaker.PositionOpen) {
			break
		}
	}
	if pos, _ := sim.Position(); pos != breaker.PositionOpen {
		t.Fatalf("Position() = %v, want Open (Test must really move the breaker)", pos)
	}

	// A later call with Test false clears test mode: the resulting update
	// must be back to ordinary GOOD quality.
	resp, err = client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 1, Select: true, Execute: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}
	for {
		update, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		points := dataPointMap(t, update)
		pos, ok := points["test.position"]
		if !ok {
			continue
		}
		if pos.GetQuality() != telemetry.DataPointQuality_QDS_GOOD {
			t.Errorf("test.position quality = %v after Test=false, want QDS_GOOD", pos.GetQuality())
		}
		if pos.GetValue() == float32(breaker.PositionClosed) {
			break
		}
	}
}

// TestSupervisoryControl_SelectWithTestDoesNotOperate checks that Test does
// not itself trigger execution: Select+Test with Execute false must only
// arm the selection (matching what Select alone would do), never move the
// breaker — Test changes quality marking only, not which action is taken.
func TestSupervisoryControl_SelectWithTestDoesNotOperate(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	startPos, _ := sim.Position()

	resp, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Select: true, Test: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("select+test: %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	// Give any (incorrect) mechanical operation a moment to start, then
	// confirm nothing moved.
	time.Sleep(20 * time.Millisecond)
	if pos, _ := sim.Position(); pos != startPos {
		t.Errorf("Position() = %v, want unchanged %v (Select+Test must not operate)", pos, startPos)
	}
}

// TestSupervisoryControl_Errors checks the request-validation and
// not-yet-selected error paths.
func TestSupervisoryControl_Errors(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tests := []struct {
		name string
		req  *telemetry.ScadaSupervisoryControlRequest
		want telemetry.ScadaSupervisoryControlResult
	}{
		{
			name: "unknown tag suffix",
			req:  &telemetry.ScadaSupervisoryControlRequest{Key: "test.position", Value: 0, Select: true},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_ITEM_IS_NOT_FOUND,
		},
		{
			name: "unknown breaker",
			req:  &telemetry.ScadaSupervisoryControlRequest{Key: "no-such-breaker.control", Value: 0, Select: true},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_OBJECT_IS_NOT_FOUND,
		},
		{
			name: "invalid value",
			req:  &telemetry.ScadaSupervisoryControlRequest{Key: "test.control", Value: 7, Select: true},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SUPPORTED,
		},
		{
			name: "execute without a prior select",
			req:  &telemetry.ScadaSupervisoryControlRequest{Key: "test.control", Value: 0, Execute: true},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SELECTED,
		},
		{
			name: "neither select, execute nor test",
			req:  &telemetry.ScadaSupervisoryControlRequest{Key: "test.control", Value: 0},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SUPPORTED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.SupervisoryControl(ctx, tt.req)
			if err != nil {
				t.Fatalf("SupervisoryControl: %v", err)
			}
			if resp.GetResult() != tt.want {
				t.Errorf("result = %v, want %v", resp.GetResult(), tt.want)
			}
		})
	}
}

// TestSupervisoryControl_Blocked checks that a blocked breaker rejects
// control with ERROR_BLOCKED.
func TestSupervisoryControl_Blocked(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sim.SetMode(ctx, breaker.ModeBlocked, "maintenance"); err != nil {
		t.Fatalf("SetMode(Blocked): %v", err)
	}

	resp, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Select: true, Execute: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("SupervisoryControl: %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_ERROR_BLOCKED {
		t.Errorf("result = %v, want ERROR_BLOCKED", resp.GetResult())
	}
}

// TestSupervisoryControl_IdempotentStillPushesConfirmation checks that a
// command matching the breaker's current position — a no-op, since nothing
// physically moves — still produces a Subscribe update confirming it,
// rather than leaving a subscribed client with no response at all.
func TestSupervisoryControl_IdempotentStillPushesConfirmation(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, &telemetry.SubstationRequest{Id: "unused"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := stream.Recv(); err != nil { // discard initial snapshot
		t.Fatalf("Recv (snapshot): %v", err)
	}

	if pos, _ := sim.Position(); pos != breaker.PositionClosed {
		t.Fatalf("test breaker starts Closed by convention (startTestServer); got %v", pos)
	}

	// Value 1 = Close: matches the breaker's already-Closed position.
	resp, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 1, Select: true, Execute: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("SupervisoryControl: %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	update, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv (confirmation update): %v", err)
	}
	points := dataPointMap(t, update)
	pos, ok := points["test.position"]
	if !ok {
		t.Fatalf("confirmation update missing test.position: %+v", update)
	}
	if pos.GetValue() != float32(breaker.PositionClosed) {
		t.Errorf("test.position value = %v, want %v (Closed, unchanged)", pos.GetValue(), breaker.PositionClosed)
	}
}

// TestSetMode_BlockedThenOn checks that SetMode(Blocked) makes
// SupervisoryControl reject with ERROR_BLOCKED, and SetMode(On) clears it
// again.
func TestSetMode_BlockedThenOn(t *testing.T) {
	client, sim, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.ProtectionTerminalSetMode(ctx, &telemetry.ProtectionTerminalSetModeRequest{
		Key: "test.mode", Mode: telemetry.ProtectionTerminalMode_MODE_BLOCKED, Reason: "maintenance", ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("SetMode(Blocked): %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("SetMode(Blocked) result = %v, want OK", resp.GetResult())
	}

	ctrl, err := client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Select: true, Execute: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("SupervisoryControl: %v", err)
	}
	if ctrl.GetResult() != telemetry.ScadaSupervisoryControlResult_ERROR_BLOCKED {
		t.Fatalf("SupervisoryControl result = %v, want ERROR_BLOCKED", ctrl.GetResult())
	}

	resp, err = client.ProtectionTerminalSetMode(ctx, &telemetry.ProtectionTerminalSetModeRequest{
		Key: "test.mode", Mode: telemetry.ProtectionTerminalMode_MODE_ON, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("SetMode(On): %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("SetMode(On) result = %v, want OK", resp.GetResult())
	}

	ctrl, err = client.SupervisoryControl(ctx, &telemetry.ScadaSupervisoryControlRequest{
		Key: "test.control", Value: 0, Select: true, Execute: true, ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("SupervisoryControl: %v", err)
	}
	if ctrl.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("SupervisoryControl result = %v, want OK after SetMode(On)", ctrl.GetResult())
	}
	waitForPosition(t, sim, breaker.PositionOpen)
}

// TestSetMode_PushesModeTag checks that SetMode's mode change is visible to
// Subscribe on the same "<name>.mode" tag SetMode already pushes when
// called directly on the Simulator.
func TestSetMode_PushesModeTag(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, &telemetry.SubstationRequest{Id: "unused"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := stream.Recv(); err != nil { // discard initial snapshot
		t.Fatalf("Recv (snapshot): %v", err)
	}

	resp, err := client.ProtectionTerminalSetMode(ctx, &telemetry.ProtectionTerminalSetModeRequest{
		Key: "test.mode", Mode: telemetry.ProtectionTerminalMode_MODE_BLOCKED, Reason: "maintenance", ClientId: "unit-test",
	})
	if err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if resp.GetResult() != telemetry.ScadaSupervisoryControlResult_OK {
		t.Fatalf("result = %v, want OK", resp.GetResult())
	}

	update, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	points := dataPointMap(t, update)
	mode, ok := points["test.mode"]
	if !ok {
		t.Fatalf("update missing test.mode: %+v", update)
	}
	if mode.GetValue() != float32(breaker.ModeBlocked) {
		t.Errorf("test.mode value = %v, want %v (Blocked)", mode.GetValue(), breaker.ModeBlocked)
	}
}

// TestSetMode_Errors checks the request-validation error paths.
func TestSetMode_Errors(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tests := []struct {
		name string
		req  *telemetry.ProtectionTerminalSetModeRequest
		want telemetry.ScadaSupervisoryControlResult
	}{
		{
			name: "unknown tag suffix",
			req:  &telemetry.ProtectionTerminalSetModeRequest{Key: "test.control", Mode: telemetry.ProtectionTerminalMode_MODE_BLOCKED},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_ITEM_IS_NOT_FOUND,
		},
		{
			name: "unknown breaker",
			req:  &telemetry.ProtectionTerminalSetModeRequest{Key: "no-such-breaker.mode", Mode: telemetry.ProtectionTerminalMode_MODE_BLOCKED},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_OBJECT_IS_NOT_FOUND,
		},
		{
			name: "unsupported mode (TEST/BLOCKED, not modelled)",
			req:  &telemetry.ProtectionTerminalSetModeRequest{Key: "test.mode", Mode: 4},
			want: telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SUPPORTED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.ProtectionTerminalSetMode(ctx, tt.req)
			if err != nil {
				t.Fatalf("SetMode: %v", err)
			}
			if resp.GetResult() != tt.want {
				t.Errorf("result = %v, want %v", resp.GetResult(), tt.want)
			}
		})
	}
}

// waitForPosition polls sim.Position() until it reports want, failing the
// test if it doesn't within a short deadline. The mechanism passes through
// Intermediate before settling, so a direct comparison right after Execute
// returns would be flaky.
func waitForPosition(t *testing.T, sim *breaker.Simulator, want breaker.Position) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pos, _ := sim.Position(); pos == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Position() did not reach %v in time", want)
}
