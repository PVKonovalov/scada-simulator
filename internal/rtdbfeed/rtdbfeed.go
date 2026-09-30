// Package rtdbfeed drives breakers' analog inputs (pkg/breaker.EmulatorFeed)
// from live RTDB (rdss-dms-rtdb) points — e.g. a load-flow case's branch
// current "case-1-branch-35-i-from" feeding "feeder-1.current.a/b/c" — so a
// breaker's protection reacts to real grid data instead of only to manual
// internal/faultsimserver injections.
//
// The feed is strictly read-only towards the RTDB: it calls IsExists (to
// warn about mapped keys the RTDB doesn't know) and Subscribe, and nothing
// else — in particular never GetItems, which creates missing keys. Pushing
// the simulator's own tags into the RTDB remains the job of rdss-dms-rtdb's
// rtdb-scada-grpc-client bridge.
//
// Like faultsimserver, the feed re-injects each breaker's latest composed
// Measurement every sustainInterval rather than only when an RTDB value
// changes, because the stage-51 time-overcurrent element integrates its
// operate timer over the wall-clock time between injections. A breaker is
// skipped while an override (a sustained faultsimserver injection) is
// active for it, and until its first RTDB value has arrived. While the RTDB
// stream is down, the last received values keep being fed with
// qds.QdsNoSource set on their quality.
package rtdbfeed

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/llog"
	"scada-simulator/pkg/qds"
	"scada-simulator/pkg/rtdb"
)

// sustainInterval is how often every fed breaker's latest measurement is
// re-injected — the same simulated sample rate faultsimserver uses.
const sustainInterval = 200 * time.Millisecond

// defaultReconnectInterval is used when Config.ReconnectInterval is not
// positive.
const defaultReconnectInterval = 5 * time.Second

// leaves maps every mappable internal analog tag leaf (the part of a tag
// after "<breaker-name>.", matching internal/telemetryserver's
// measurementPoints) to the Measurement field it sets.
var leaves = map[string]func(m *breaker.Measurement) *float64{
	"current.a":      func(m *breaker.Measurement) *float64 { return &m.Current.A },
	"current.b":      func(m *breaker.Measurement) *float64 { return &m.Current.B },
	"current.c":      func(m *breaker.Measurement) *float64 { return &m.Current.C },
	"voltage.a":      func(m *breaker.Measurement) *float64 { return &m.Voltage.A },
	"voltage.b":      func(m *breaker.Measurement) *float64 { return &m.Voltage.B },
	"voltage.c":      func(m *breaker.Measurement) *float64 { return &m.Voltage.C },
	"power.active":   func(m *breaker.Measurement) *float64 { return &m.ActivePower },
	"power.reactive": func(m *breaker.Measurement) *float64 { return &m.ReactivePower },
	"power.apparent": func(m *breaker.Measurement) *float64 { return &m.ApparentPower },
	"frequency":      func(m *breaker.Measurement) *float64 { return &m.Frequency },
}

// Config configures a Feed's RTDB connection.
type Config struct {
	// Addr is the RTDB gRPC server's "host:port".
	Addr string
	// ClientID identifies this simulator in the RTDB's logs.
	ClientID string
	// ReconnectInterval is the wait between connection attempts, both
	// initially and after a broken stream; defaultReconnectInterval if <= 0.
	ReconnectInterval time.Duration
}

// Mappings maps a breaker name to its leaf → RTDB key mapping (see
// internal/configuration.BreakerConfig.RtdbMapping).
type Mappings map[string]map[string]string

// Validate checks every mapping names a configured breaker (in breakers),
// only uses known leaves, and has no empty RTDB key. It also returns, as
// warnings, every RTDB key that looks like one of the simulator's own tags
// ("<configured-breaker-name>.…"): such a key is most likely fed back into
// the RTDB from this very simulator by the bridge, forming a loop.
func (ms Mappings) Validate(breakers map[string]*breaker.Simulator) (warnings []string, err error) {
	for _, name := range slices.Sorted(maps.Keys(ms)) {
		if _, ok := breakers[name]; !ok {
			return nil, fmt.Errorf("rtdb_mapping for unknown breaker %q", name)
		}
		for _, leaf := range slices.Sorted(maps.Keys(ms[name])) {
			key := ms[name][leaf]
			if _, ok := leaves[leaf]; !ok {
				return nil, fmt.Errorf("breaker %q: rtdb_mapping: unknown leaf %q (known: %s)",
					name, leaf, strings.Join(slices.Sorted(maps.Keys(leaves)), ", "))
			}
			if key == "" {
				return nil, fmt.Errorf("breaker %q: rtdb_mapping: leaf %q has an empty RTDB key", name, leaf)
			}
			for other := range breakers {
				if strings.HasPrefix(key, other+".") {
					warnings = append(warnings, fmt.Sprintf(
						"breaker %q: rtdb_mapping %s -> %q looks like this simulator's own tag; if the bridge feeds it back into the RTDB, values will loop",
						name, leaf, key))
					break
				}
			}
		}
	}
	return warnings, nil
}

// target is one Measurement field fed by an RTDB key.
type target struct {
	breaker string // breaker name
	leaf    string // leaf name, a key of leaves
}

// breakerState is one breaker's composed measurement, built up from every
// RTDB update received for its mapped leaves.
type breakerState struct {
	sim      *breaker.Simulator
	m        breaker.Measurement    // latest value of every mapped leaf; Quality/Timestamp recomputed on each update
	quality  map[string]qds.Quality // latest RTDB quality per received leaf
	received bool                   // at least one mapped leaf has been received; nothing is injected before
}

// Feed subscribes to every mapped RTDB key and feeds the values into the
// mapped breakers — see the package doc comment.
type Feed struct {
	cfg      Config
	override func(name string) bool // reports whether another source currently owns the breaker; nil = never
	logger   *llog.LevelLog         // logger for connection and update activity

	targets map[string][]target // RTDB key -> every leaf it feeds
	keys    []string            // sorted keys of targets

	mu     sync.Mutex               // guards states and stale
	states map[string]*breakerState // per mapped breaker, keyed by name
	stale  bool                     // the RTDB stream is currently down; QdsNoSource is ORed onto every quality
}

// New builds a Feed for mappings over breakers. mappings must have passed
// Mappings.Validate against breakers. override, if non-nil, is consulted
// before every injection (see faultsimserver.FaultSimulatorServer.Overriding).
func New(cfg Config, mappings Mappings, breakers map[string]*breaker.Simulator, override func(name string) bool, logger *llog.LevelLog) *Feed {
	if cfg.ReconnectInterval <= 0 {
		cfg.ReconnectInterval = defaultReconnectInterval
	}
	f := &Feed{
		cfg:      cfg,
		override: override,
		logger:   logger,
		targets:  make(map[string][]target),
		states:   make(map[string]*breakerState),
		stale:    true,
	}
	for name, mapping := range mappings {
		if len(mapping) == 0 {
			continue
		}
		f.states[name] = &breakerState{sim: breakers[name], quality: make(map[string]qds.Quality)}
		for leaf, key := range mapping {
			f.targets[key] = append(f.targets[key], target{breaker: name, leaf: leaf})
		}
	}
	f.keys = slices.Sorted(maps.Keys(f.targets))
	return f
}

// Handles reports whether the named breaker is fed by f — the
// faultsimserver.FaultSimulatorServer.WithHandoff predicate.
func (f *Feed) Handles(name string) bool {
	_, ok := f.states[name] // states' key set never changes after New
	return ok
}

// Run connects to the RTDB and feeds breakers until ctx is done,
// reconnecting every Config.ReconnectInterval whenever the connection
// can't be established or the stream breaks. It returns only once ctx is
// done (or immediately if nothing is mapped).
func (f *Feed) Run(ctx context.Context) {
	if len(f.keys) == 0 {
		return
	}

	conn, err := grpc.NewClient(f.cfg.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		// Only a malformed target fails here; NewClient doesn't dial.
		f.logger.Errorf("rtdbfeed: invalid RTDB address %q: %v", f.cfg.Addr, err)
		return
	}
	defer conn.Close()
	client := rtdb.NewRtdbClient(conn)

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { f.sustain(ctx) })

	f.logger.Infof("rtdbfeed: feeding %d breaker(s) from %d RTDB key(s) at %s", len(f.states), len(f.keys), f.cfg.Addr)
	for {
		err := f.stream(ctx, client)
		f.setStale()
		if ctx.Err() != nil {
			return
		}
		f.logger.Warnf("rtdbfeed: RTDB stream at %s unavailable: %v; retrying in %s", f.cfg.Addr, err, f.cfg.ReconnectInterval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(f.cfg.ReconnectInterval):
		}
	}
}

// stream runs one RTDB session: warns about mapped keys the RTDB doesn't
// have, subscribes to every mapped key, and applies every update received
// until the stream fails or ctx is done. It always returns a non-nil error.
func (f *Feed) stream(ctx context.Context, client rtdb.RtdbClient) error {
	for _, key := range f.keys {
		resp, err := client.IsExists(ctx, &rtdb.RTDBItemExistsRequest{Key: key})
		if err != nil {
			return fmt.Errorf("IsExists: %w", err)
		}
		if !resp.GetExists() {
			f.logger.Warnf("rtdbfeed: RTDB key %q does not exist (yet); its leaves stay unfed until it appears", key)
		}
	}

	options := make([]*rtdb.RTDBSubscribeItemOptions, 0, len(f.keys))
	for _, key := range f.keys {
		// OFF: every update is delivered, not only changes beyond a delta.
		options = append(options, &rtdb.RTDBSubscribeItemOptions{Key: key, ControlType: rtdb.RTDBSubscribeDataControl_OFF})
	}
	sub, err := client.Subscribe(ctx, &rtdb.RTDBSubscribeRequest{ItemOptions: options, ClientId: f.cfg.ClientID})
	if err != nil {
		return fmt.Errorf("Subscribe: %w", err)
	}

	first := true
	for {
		resp, err := sub.Recv()
		if err != nil {
			return fmt.Errorf("Recv: %w", err)
		}
		if first {
			// Only a received message proves the session is really up.
			f.logger.Infof("rtdbfeed: subscribed to %d RTDB key(s) at %s", len(f.keys), f.cfg.Addr)
			first = false
		}
		f.apply(resp.GetItems())
	}
}

// apply stores every mapped item's value and quality into its breakers'
// measurements, clears the stale flag, and immediately injects each
// affected breaker (rather than waiting for the next sustain tick).
func (f *Feed) apply(items []*rtdb.RTDBItem) {
	f.mu.Lock()
	f.stale = false
	touched := make(map[string]bool)
	for _, item := range items {
		data := item.GetData()
		for _, t := range f.targets[item.GetKey()] {
			st := f.states[t.breaker]
			*leaves[t.leaf](&st.m) = data.GetValue()
			st.quality[t.leaf] = qds.Quality(data.GetQuality())
			if ts := data.GetTimestamp(); ts != nil {
				st.m.Timestamp = ts.AsTime()
			}
			st.received = true
			touched[t.breaker] = true
			f.logger.Tracef("rtdbfeed: %s -> %s.%s = %v quality=%#x", item.GetKey(), t.breaker, t.leaf, data.GetValue(), data.GetQuality())
		}
	}
	injections := f.injectionsLocked(func(name string) bool { return touched[name] })
	f.mu.Unlock()

	f.inject(injections)
}

// setStale marks the RTDB stream as down and pushes the resulting
// QdsNoSource quality to every fed breaker right away.
func (f *Feed) setStale() {
	f.mu.Lock()
	f.stale = true
	injections := f.injectionsLocked(func(string) bool { return true })
	f.mu.Unlock()

	f.inject(injections)
}

// sustain re-injects every fed breaker's latest measurement every
// sustainInterval until ctx is done.
func (f *Feed) sustain(ctx context.Context) {
	ticker := time.NewTicker(sustainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			f.mu.Lock()
			injections := f.injectionsLocked(func(string) bool { return true })
			f.mu.Unlock()
			f.inject(injections)
		case <-ctx.Done():
			return
		}
	}
}

// injection is one pending Measurement for one breaker.
type injection struct {
	name string
	sim  *breaker.Simulator
	m    breaker.Measurement
}

// injectionsLocked composes the current Measurement of every breaker that
// matches include and has received data, with Quality the OR of every
// received leaf's quality (plus QdsNoSource while stale). f.mu must be held.
func (f *Feed) injectionsLocked(include func(name string) bool) []injection {
	var out []injection
	for name, st := range f.states {
		if !st.received || !include(name) {
			continue
		}
		m := st.m
		m.Quality = qds.QdsGood
		for _, q := range st.quality {
			m.Quality |= q
		}
		if f.stale {
			m.Quality |= qds.QdsNoSource
		}
		if m.Timestamp.IsZero() {
			m.Timestamp = time.Now()
		}
		out = append(out, injection{name: name, sim: st.sim, m: m})
	}
	return out
}

// inject feeds each injection to its breaker, skipping any breaker another
// source currently overrides. Called without f.mu held, since
// Simulator.InjectMeasurement blocks on the breaker's run loop.
func (f *Feed) inject(injections []injection) {
	for _, in := range injections {
		if f.override != nil && f.override(in.name) {
			continue
		}
		_ = in.sim.InjectMeasurement(in.m) // always returns nil; see EmulatorFeed.InjectMeasurement
	}
}
