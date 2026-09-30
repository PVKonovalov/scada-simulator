package rtdbfeed

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/llog"
	"scada-simulator/pkg/qds"
	"scada-simulator/pkg/rtdb"
)

// fakeRtdb is a minimal read-only RTDB: IsExists reports every key in
// items, and Subscribe sends items once, then every batch pushed on push.
type fakeRtdb struct {
	rtdb.UnimplementedRtdbServer

	items map[string]float64       // initial snapshot, key -> value
	push  chan []*rtdb.RTDBItem    // later updates
	subs  atomic.Int32             // number of Subscribe calls served
	last  atomic.Pointer[[]string] // keys of the latest Subscribe request
}

// IsExists implements rtdb.RtdbServer.
func (f *fakeRtdb) IsExists(_ context.Context, req *rtdb.RTDBItemExistsRequest) (*rtdb.RTDBItemExistsResponse, error) {
	_, ok := f.items[req.GetKey()]
	return &rtdb.RTDBItemExistsResponse{Exists: ok}, nil
}

// Subscribe implements rtdb.RtdbServer.
func (f *fakeRtdb) Subscribe(req *rtdb.RTDBSubscribeRequest, stream grpc.ServerStreamingServer[rtdb.RTDBItemsResponse]) error {
	f.subs.Add(1)
	keys := make([]string, 0, len(req.GetItemOptions()))
	var initial []*rtdb.RTDBItem
	for _, o := range req.GetItemOptions() {
		keys = append(keys, o.GetKey())
		if v, ok := f.items[o.GetKey()]; ok {
			initial = append(initial, item(o.GetKey(), v, qds.QdsGood))
		}
	}
	f.last.Store(&keys)
	if err := stream.Send(&rtdb.RTDBItemsResponse{Items: initial}); err != nil {
		return err
	}
	for {
		select {
		case batch := <-f.push:
			if err := stream.Send(&rtdb.RTDBItemsResponse{Items: batch}); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

// item builds an RTDBItem with the current time as its timestamp.
func item(key string, value float64, q qds.Quality) *rtdb.RTDBItem {
	return &rtdb.RTDBItem{Key: key, Data: &rtdb.RTDBDataValue{Value: value, Quality: uint32(q), Timestamp: timestamppb.Now()}}
}

// serve starts fake on addr ("127.0.0.1:0" for any port) and returns the
// server and its actual address.
func serve(t *testing.T, addr string, fake *fakeRtdb) (*grpc.Server, string) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	rtdb.RegisterRtdbServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	return srv, lis.Addr().String()
}

// newSim builds a Closed breaker whose protection stays well above any
// value these tests feed, so feeding never trips it.
func newSim(ctx context.Context, name string) *breaker.Simulator {
	return breaker.New(ctx, breaker.Config{
		Name:                  name,
		InitialPosition:       breaker.PositionClosed,
		MechanicalOperateTime: time.Millisecond,
		SettingsGroups: []breaker.SettingsGroupConfig{{
			Group:    0,
			Settings: breaker.ProtectionSettings{InstantaneousPickup: 1e9},
		}},
	})
}

// waitFor polls cond until it holds or a 3s deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMappings_Validate checks each class of invalid mapping is rejected
// and that a key named like a simulator tag only warns.
func TestMappings_Validate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sims := map[string]*breaker.Simulator{"feeder-1": newSim(ctx, "feeder-1")}

	tests := []struct {
		name     string
		mappings Mappings
		wantErr  string
		wantWarn int
	}{
		{"valid", Mappings{"feeder-1": {"current.a": "case-1-branch-35-i-from", "power.active": "p"}}, "", 0},
		{"unknown breaker", Mappings{"feeder-9": {"current.a": "k"}}, "unknown breaker", 0},
		{"unknown leaf", Mappings{"feeder-1": {"current.d": "k"}}, "unknown leaf", 0},
		{"empty key", Mappings{"feeder-1": {"current.a": ""}}, "empty RTDB key", 0},
		{"loop warning", Mappings{"feeder-1": {"current.a": "feeder-1.current.a"}}, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := tt.mappings.Validate(sims)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if len(warnings) != tt.wantWarn {
				t.Errorf("warnings = %v, want %d", warnings, tt.wantWarn)
			}
		})
	}
}

// TestFeed_FeedsMappedLeaves checks one RTDB key fans out to every leaf
// mapped to it, updates are applied, and RTDB quality is passed through.
func TestFeed_FeedsMappedLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sim := newSim(ctx, "feeder-1")
	defer sim.Close()

	fake := &fakeRtdb{items: map[string]float64{"i-from": 350, "p": 1.5e6}, push: make(chan []*rtdb.RTDBItem)}
	srv, addr := serve(t, "127.0.0.1:0", fake)
	defer srv.Stop()

	feed := New(Config{Addr: addr, ReconnectInterval: 50 * time.Millisecond},
		Mappings{"feeder-1": {"current.a": "i-from", "current.b": "i-from", "current.c": "i-from", "power.active": "p"}},
		map[string]*breaker.Simulator{"feeder-1": sim}, nil, llog.Logger)
	if !feed.Handles("feeder-1") || feed.Handles("feeder-2") {
		t.Errorf("Handles: want true for feeder-1 only")
	}
	done := make(chan struct{})
	go func() { feed.Run(ctx); close(done) }()

	waitFor(t, "initial snapshot", func() bool {
		m := sim.Measurement()
		return m.Current == breaker.PhaseValues{A: 350, B: 350, C: 350} && m.ActivePower == 1.5e6 && m.Quality == qds.QdsGood
	})

	fake.push <- []*rtdb.RTDBItem{item("i-from", 420, qds.QdsQuestionable)}
	waitFor(t, "update with quality", func() bool {
		m := sim.Measurement()
		return m.Current.A == 420 && m.Current.C == 420 && m.Quality == qds.QdsQuestionable
	})

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestFeed_Override checks nothing is injected while override is true and
// feeding resumes as soon as it turns false.
func TestFeed_Override(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sim := newSim(ctx, "feeder-1")
	defer sim.Close()

	fake := &fakeRtdb{items: map[string]float64{"i-from": 350}, push: make(chan []*rtdb.RTDBItem)}
	srv, addr := serve(t, "127.0.0.1:0", fake)
	defer srv.Stop()

	var overriding atomic.Bool
	overriding.Store(true)
	feed := New(Config{Addr: addr}, Mappings{"feeder-1": {"current.a": "i-from"}},
		map[string]*breaker.Simulator{"feeder-1": sim}, func(string) bool { return overriding.Load() }, llog.Logger)
	go feed.Run(ctx)

	waitFor(t, "subscription", func() bool { return fake.subs.Load() == 1 })
	time.Sleep(3 * sustainInterval)
	if got := sim.Measurement().Current.A; got != 0 {
		t.Fatalf("current.a = %v while overridden, want 0", got)
	}

	overriding.Store(false)
	waitFor(t, "feed after override ends", func() bool { return sim.Measurement().Current.A == 350 })
}

// TestFeed_Reconnect checks a broken stream marks values QdsNoSource and
// the feed resubscribes and clears it once the RTDB is back.
func TestFeed_Reconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sim := newSim(ctx, "feeder-1")
	defer sim.Close()

	fake := &fakeRtdb{items: map[string]float64{"i-from": 350}, push: make(chan []*rtdb.RTDBItem)}
	srv, addr := serve(t, "127.0.0.1:0", fake)

	feed := New(Config{Addr: addr, ReconnectInterval: 50 * time.Millisecond}, Mappings{"feeder-1": {"current.a": "i-from"}},
		map[string]*breaker.Simulator{"feeder-1": sim}, nil, llog.Logger)
	go feed.Run(ctx)
	waitFor(t, "initial value", func() bool { return sim.Measurement().Current.A == 350 })

	srv.Stop()
	waitFor(t, "QdsNoSource after stream loss", func() bool { return sim.Measurement().Quality.Has(qds.QdsNoSource) })
	if got := sim.Measurement().Current.A; got != 350 {
		t.Errorf("current.a = %v while stale, want last value 350 held", got)
	}

	fake.items["i-from"] = 360
	srv2, _ := serve(t, addr, fake)
	defer srv2.Stop()
	waitFor(t, "fresh value after reconnect", func() bool {
		m := sim.Measurement()
		return m.Current.A == 360 && m.Quality == qds.QdsGood
	})
	if got := *fake.last.Load(); len(got) != 1 || got[0] != "i-from" {
		t.Errorf("subscribed keys = %v, want [i-from]", got)
	}
}
