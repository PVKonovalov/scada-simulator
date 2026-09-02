package telemetryserver

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/llog"
	"scada-simulator/pkg/telemetry"
)

// startTestServer builds a Simulator named "test", serves it over a real
// TCP listener on an OS-assigned port, and returns a connected
// TelemetryStreamClient plus a cleanup func.
func startTestServer(t *testing.T) (telemetry.TelemetryStreamClient, *breaker.Simulator, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	sim := breaker.New(ctx, breaker.Config{
		Name:                  "test",
		InitialPosition:       breaker.PositionClosed,
		MechanicalOperateTime: time.Millisecond,
	})

	grpcServer := grpc.NewServer()
	telemetry.RegisterTelemetryStreamServer(grpcServer, NewTelemetryServer(
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
	return telemetry.NewTelemetryStreamClient(conn), sim, cleanup
}

// dataPointMap collects a SubstationUpdate's points into a lookup by key,
// failing the test if a duplicate key appears within the same update.
func dataPointMap(t *testing.T, update *telemetry.SubstationUpdate) map[string]*telemetry.DataPoint {
	t.Helper()
	m := make(map[string]*telemetry.DataPoint, len(update.GetPoints()))
	for _, p := range update.GetPoints() {
		if _, ok := m[p.GetKey()]; ok {
			t.Fatalf("duplicate key %q in one SubstationUpdate", p.GetKey())
		}
		m[p.GetKey()] = p
	}
	return m
}

// TestTelemetryServer_InitialSnapshot checks that Subscribe's first message
// carries every documented tag for the breaker, correctly typed.
func TestTelemetryServer_InitialSnapshot(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, &telemetry.SubstationRequest{Id: "unused"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	update, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	points := dataPointMap(t, update)

	wantKeys := []string{
		"test.position", "test.mode",
		"test.current.a", "test.current.b", "test.current.c",
		"test.voltage.a", "test.voltage.b", "test.voltage.c",
		"test.power.active", "test.power.reactive", "test.power.apparent",
		"test.frequency",
		"test.protection.state", "test.protection.group",
		"test.autoreclose.state", "test.autoreclose.attempt",
	}
	for _, key := range wantKeys {
		if _, ok := points[key]; !ok {
			t.Errorf("initial snapshot missing tag %q", key)
		}
	}

	pos := points["test.position"]
	if pos.GetType() != telemetry.DataPointType_INTEGER {
		t.Errorf("test.position type = %v, want INTEGER", pos.GetType())
	}
	if pos.GetValue() != float32(breaker.PositionClosed) {
		t.Errorf("test.position value = %v, want %v (Closed)", pos.GetValue(), breaker.PositionClosed)
	}

	mode := points["test.mode"]
	if mode.GetType() != telemetry.DataPointType_BOOLEAN || mode.GetValue() != float32(breaker.ModeOn) {
		t.Errorf("test.mode = (%v, %v), want (BOOLEAN, %v)", mode.GetType(), mode.GetValue(), breaker.ModeOn)
	}
}

// TestTelemetryServer_MeasurementUpdate checks that injecting a measurement
// produces a follow-up SubstationUpdate with the expected analog values.
func TestTelemetryServer_MeasurementUpdate(t *testing.T) {
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

	if err := sim.InjectMeasurement(breaker.Measurement{
		Current:   breaker.PhaseValues{A: 111, B: 222, C: 333},
		Frequency: 50.5,
	}); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}

	update, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv (measurement update): %v", err)
	}
	points := dataPointMap(t, update)

	if got := points["test.current.a"].GetValue(); got != 111 {
		t.Errorf("test.current.a = %v, want 111", got)
	}
	if got := points["test.current.b"].GetValue(); got != 222 {
		t.Errorf("test.current.b = %v, want 222", got)
	}
	if got := points["test.frequency"].GetValue(); got != float32(50.5) {
		t.Errorf("test.frequency = %v, want 50.5", got)
	}
}

// TestTelemetryServer_PositionUpdate checks that a control operation's
// resulting position change is pushed as a follow-up update.
func TestTelemetryServer_PositionUpdate(t *testing.T) {
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

	cmd := breaker.Command{Target: breaker.PositionOpen, Source: "test"}
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Fatalf("Operate: %v", err)
	}

	// The mechanism passes through Intermediate before settling Open;
	// keep reading updates until position finally reports Open. None of
	// these updates should carry test.mode: an ordinary position change is
	// not a mode change (see dataPointsForEvent's comment).
	for {
		update, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		points := dataPointMap(t, update)
		if _, ok := points["test.mode"]; ok {
			t.Errorf("update carries test.mode during an ordinary position change: %+v", update)
		}
		pos, ok := points["test.position"]
		if !ok {
			continue
		}
		if pos.GetValue() == float32(breaker.PositionOpen) {
			return
		}
	}
}

// TestTelemetryServer_SetModePushesModeTag checks that SetMode pushes a
// test.mode update on its own, independent of any position change.
func TestTelemetryServer_SetModePushesModeTag(t *testing.T) {
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

	if err := sim.SetMode(ctx, breaker.ModeBlocked, "maintenance"); err != nil {
		t.Fatalf("SetMode(Blocked): %v", err)
	}
	blocked := waitForModeUpdate(t, stream)
	if blocked.GetValue() != float32(breaker.ModeBlocked) {
		t.Errorf("test.mode after SetMode(Blocked) = %v, want %v", blocked.GetValue(), breaker.ModeBlocked)
	}

	if err := sim.SetMode(ctx, breaker.ModeOn, ""); err != nil {
		t.Fatalf("SetMode(On): %v", err)
	}
	on := waitForModeUpdate(t, stream)
	if on.GetValue() != float32(breaker.ModeOn) {
		t.Errorf("test.mode after SetMode(On) = %v, want %v", on.GetValue(), breaker.ModeOn)
	}
}

// waitForModeUpdate reads updates from stream until one carries test.mode,
// failing the test if the stream ends first.
func waitForModeUpdate(t *testing.T, stream telemetry.TelemetryStream_SubscribeClient) *telemetry.DataPoint {
	t.Helper()
	for {
		update, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		points := dataPointMap(t, update)
		if p, ok := points["test.mode"]; ok {
			return p
		}
	}
}
