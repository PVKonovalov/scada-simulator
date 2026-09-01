// Package telemetryserver implements the gRPC TelemetryStream service
// (api/scada/telemetry.proto) on top of one or more pkg/breaker.Simulator
// instances.
package telemetryserver

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/llog"
	"scada-simulator/pkg/qds"
	"scada-simulator/pkg/telemetry"
)

// liveEventKinds are the breaker.Event kinds forward listens for in order
// to keep every connected client's stream current after its initial
// snapshot. EventControlRejected is deliberately excluded: a rejected
// command changes no tag. EventControlBlocked/EventControlUnblocked are the
// only source of "<name>.blocked" updates (see dataPointsForEvent) —
// Block/Unblock also emit EventPositionChanged, but that alone must not
// push ".blocked" too, or every ordinary position change (which also
// carries a Quality) would resend an unchanged ".blocked" value.
var liveEventKinds = []breaker.EventKind{
	breaker.EventPositionChanged,
	breaker.EventMeasurementChanged,
	breaker.EventProtectionPickedUp,
	breaker.EventTrip,
	breaker.EventAutoRecloseAttempt,
	breaker.EventLockout,
	breaker.EventSettingsChanged,
	breaker.EventControlBlocked,
	breaker.EventControlUnblocked,
}

// clientBuffer is how many updates a connected client's own channel
// buffers before broadcast starts dropping the oldest queued one in favor
// of the newest — a slow client must not block the shared forward
// goroutine, or delay delivery to every other client.
const clientBuffer = 32

// TelemetryServer implements telemetry.TelemetryStreamServer, bridging
// breakers to SCADA tags per the naming convention documented in
// pkg/breaker/README.md: "<breaker-name>.<category>.<leaf>".
//
// Exactly one forward goroutine runs per configured breaker for the
// server's whole lifetime (started by NewTelemetryServer), regardless of
// how many Subscribe clients are connected — zero, one, or many. Each
// breaker event is converted to tags and logged there exactly once, then
// broadcast to every currently connected client's own channel; Subscribe
// itself does no event-to-tag conversion or logging of its own, only
// per-client registration and stream.Send.
type TelemetryServer struct {
	// UnimplementedTelemetryStreamServer satisfies the forward-compatibility
	// requirement of telemetry.TelemetryStreamServer.
	telemetry.UnimplementedTelemetryStreamServer

	breakers map[string]*breaker.Simulator // configured breakers, keyed by Config.Name
	logger   *llog.LevelLog                // logger for subscribe/unsubscribe, control activity, and forwarded updates

	pendingMu sync.Mutex                     // guards pending
	pending   map[string]breaker.SelectionID // in-flight SupervisoryControl selections, keyed by request Key; see SupervisoryControl

	subMu       sync.Mutex                               // guards subscribers/nextSubID
	subscribers map[int]chan *telemetry.SubstationUpdate // connected Subscribe clients' own output channels, keyed by an internal id
	nextSubID   int                                      // next id to hand out in subscribers

	testMu   sync.Mutex      // guards testMode
	testMode map[string]bool // whether each breaker (keyed by Config.Name) is currently in SupervisoryControl test mode; see SupervisoryControl and isTestMode
}

// isTestMode reports whether name's breaker is currently in test mode (set
// by a SupervisoryControl call with Test true, cleared by one with Test
// false — see SupervisoryControl).
func (s *TelemetryServer) isTestMode(name string) bool {
	s.testMu.Lock()
	defer s.testMu.Unlock()
	return s.testMode[name]
}

// setTestMode records whether name's breaker is in test mode.
func (s *TelemetryServer) setTestMode(name string, on bool) {
	s.testMu.Lock()
	s.testMode[name] = on
	s.testMu.Unlock()
}

// applyTestMode ORs qds.QdsTest onto q when testMode is true, so every tag
// for a breaker currently in SupervisoryControl test mode reports it —
// see SupervisoryControl's Test handling.
func applyTestMode(q qds.Quality, testMode bool) qds.Quality {
	if testMode {
		q.Set(qds.QdsTest)
	}
	return q
}

// NewTelemetryServer builds a TelemetryServer over breakers, keyed by each
// Simulator's configured name, and starts its per-breaker forward
// goroutines. ctx should be the application's overall lifetime context —
// the same one breakers were themselves constructed with — since that is
// what stops the forward goroutines (via sim.Subscribe(ctx, ...)'s channel
// closing) when the application shuts down.
func NewTelemetryServer(ctx context.Context, breakers map[string]*breaker.Simulator, logger *llog.LevelLog) *TelemetryServer {
	s := &TelemetryServer{
		breakers:    breakers,
		logger:      logger,
		pending:     make(map[string]breaker.SelectionID),
		subscribers: make(map[int]chan *telemetry.SubstationUpdate),
		testMode:    make(map[string]bool),
	}
	for name, sim := range breakers {
		events, err := sim.Subscribe(ctx, liveEventKinds...)
		if err != nil {
			continue // breaker already closed; nothing to forward for it
		}
		go s.forward(name, sim, events)
	}
	return s
}

// forward is the single, long-lived event-to-tag converter for one
// breaker; see TelemetryServer's doc comment.
func (s *TelemetryServer) forward(name string, sim *breaker.Simulator, events <-chan breaker.Event) {
	for ev := range events {
		points := dataPointsForEvent(name, sim, ev, s.isTestMode(name))
		if len(points) == 0 {
			continue
		}
		update := &telemetry.SubstationUpdate{Points: points}
		s.logUpdate("update", update)
		s.broadcast(update)
	}
}

// broadcast sends update to every currently connected client.
func (s *TelemetryServer) broadcast(update *telemetry.SubstationUpdate) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ch := range s.subscribers {
		trySend(ch, update)
	}
}

// trySend delivers update to ch without blocking, dropping the oldest
// queued update to make room if ch is full.
func trySend(ch chan *telemetry.SubstationUpdate, update *telemetry.SubstationUpdate) {
	select {
	case ch <- update:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- update:
	default:
	}
}

// Subscribe implements telemetry.TelemetryStreamServer. It streams tags for
// every configured breaker: SubstationRequest.Id is not used to filter
// them, since this simulator does not model a client/substation hierarchy
// — every subscriber sees every emulated breaker. The stream opens with one
// SubstationUpdate carrying a full snapshot of every tag's current value,
// then relays whatever the shared forward goroutines broadcast, until the
// client disconnects or the server shuts down.
func (s *TelemetryServer) Subscribe(req *telemetry.SubstationRequest, stream telemetry.TelemetryStream_SubscribeServer) error {
	ctx := stream.Context()
	s.logger.Infof("telemetry: subscribe start (id=%q)", req.GetId())
	defer s.logger.Infof("telemetry: subscribe end (id=%q)", req.GetId())

	if err := s.sendSnapshot(stream); err != nil {
		return err
	}

	ch := make(chan *telemetry.SubstationUpdate, clientBuffer)
	s.subMu.Lock()
	id := s.nextSubID
	s.nextSubID++
	s.subscribers[id] = ch
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subscribers, id)
		s.subMu.Unlock()
	}()

	for {
		select {
		case update := <-ch:
			if err := stream.Send(update); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// sendSnapshot logs and sends a fresh full-status snapshot. Unlike
// forward's updates, a snapshot is deliberately logged once per connecting
// client, not deduplicated: each client's connection is itself a distinct
// occurrence, not a rebroadcast of something already logged elsewhere.
func (s *TelemetryServer) sendSnapshot(stream telemetry.TelemetryStream_SubscribeServer) error {
	update := s.snapshot()
	s.logUpdate("snapshot", update)
	return stream.Send(update)
}

// logUpdate logs every point in update at Trace level (label distinguishes
// a snapshot from a forwarded update in the log).
func (s *TelemetryServer) logUpdate(label string, update *telemetry.SubstationUpdate) {
	for _, p := range update.GetPoints() {
		s.logger.Tracef("telemetry: %s %s type=%s value=%.2f quality=%s ts=%s",
			label, p.GetKey(), p.GetType(), p.GetValue(), p.GetQuality(), p.GetTimestamp().AsTime().Format(time.RFC3339Nano))
	}
}

// snapshot builds a SubstationUpdate carrying every tag's current value for
// every configured breaker.
func (s *TelemetryServer) snapshot() *telemetry.SubstationUpdate {
	var points []*telemetry.DataPoint
	for name, sim := range s.breakers {
		points = append(points, breakerSnapshot(name, sim, s.isTestMode(name))...)
	}
	return &telemetry.SubstationUpdate{Points: points}
}

// breakerSnapshot returns every tag's current value for one breaker.
// testMode marks every tag qds.QdsTest when true — see applyTestMode.
func breakerSnapshot(name string, sim *breaker.Simulator, testMode bool) []*telemetry.DataPoint {
	pos, posQuality := sim.Position()
	posQuality = applyTestMode(posQuality, testMode)
	now := time.Now()

	points := []*telemetry.DataPoint{
		intPoint(name+".position", int(pos), posQuality, now),
		boolPoint(name+".blocked", posQuality.Has(qds.QdsBlocked), posQuality, now),
	}
	points = append(points, measurementPoints(name, sim.Measurement(), testMode)...)
	points = append(points, statusPoints(name, sim, now, testMode)...)
	return points
}

// dataPointsForEvent maps one breaker.Event to the tags it should refresh,
// or nil if the event carries no tag update of its own. testMode marks
// every tag qds.QdsTest when true — see applyTestMode. Note this only
// refreshes "<name>.position" itself on a real position change —
// "<name>.blocked" is refreshed solely by EventControlBlocked/
// EventControlUnblocked (via sim.Position()'s quality, since neither
// event's Detail carries one of its own), even though Block/Unblock also
// emit EventPositionChanged: an ordinary position change is not a blocked-
// state change, and must not resend an unchanged ".blocked" value.
func dataPointsForEvent(name string, sim *breaker.Simulator, ev breaker.Event, testMode bool) []*telemetry.DataPoint {
	switch d := ev.Detail.(type) {
	case breaker.PositionChangedDetail:
		return []*telemetry.DataPoint{
			intPoint(name+".position", int(d.Position), applyTestMode(d.Quality, testMode), ev.Timestamp),
		}
	case breaker.Measurement:
		return measurementPoints(name, d, testMode)
	case breaker.ControlBlockedDetail, breaker.ControlUnblockedDetail:
		_, quality := sim.Position()
		quality = applyTestMode(quality, testMode)
		return []*telemetry.DataPoint{boolPoint(name+".blocked", quality.Has(qds.QdsBlocked), quality, ev.Timestamp)}
	default:
		// EventProtectionPickedUp/Trip/AutoRecloseAttempt/Lockout/
		// SettingsChanged: rather than hand-decode each Detail type, just
		// re-read the current protection/autoreclose status — cheap, and
		// always exactly right.
		return statusPoints(name, sim, ev.Timestamp, testMode)
	}
}

// measurementPoints converts a Measurement into its analog tags. testMode
// marks every tag qds.QdsTest when true — see applyTestMode.
func measurementPoints(name string, m breaker.Measurement, testMode bool) []*telemetry.DataPoint {
	ts := m.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	quality := applyTestMode(m.Quality, testMode)
	return []*telemetry.DataPoint{
		floatPoint(name+".current.a", m.Current.A, quality, ts),
		floatPoint(name+".current.b", m.Current.B, quality, ts),
		floatPoint(name+".current.c", m.Current.C, quality, ts),
		floatPoint(name+".voltage.a", m.Voltage.A, quality, ts),
		floatPoint(name+".voltage.b", m.Voltage.B, quality, ts),
		floatPoint(name+".voltage.c", m.Voltage.C, quality, ts),
		floatPoint(name+".power.active", m.ActivePower, quality, ts),
		floatPoint(name+".power.reactive", m.ReactivePower, quality, ts),
		floatPoint(name+".power.apparent", m.ApparentPower, quality, ts),
		floatPoint(name+".frequency", m.Frequency, quality, ts),
	}
}

// statusPoints reads sim's current protection/autoreclose status and
// converts it into its tags. These are the simulator's own computed state
// rather than an external reading, so they carry qds.QdsGood quality
// (plus qds.QdsTest when testMode is true — see applyTestMode).
func statusPoints(name string, sim *breaker.Simulator, ts time.Time, testMode bool) []*telemetry.DataPoint {
	ps := sim.ProtectionStatus()
	ar := sim.AutoRecloseStatus()
	quality := applyTestMode(qds.QdsGood, testMode)
	return []*telemetry.DataPoint{
		diPoint(name+".protection.state", int(ps.State), quality, ts),
		diPoint(name+".protection.group", int(ps.ActiveGroup)+1, quality, ts), // 1-based, matching pkg/breaker's YAML convention
		diPoint(name+".autoreclose.state", int(ar.State), quality, ts),
		diPoint(name+".autoreclose.attempt", ar.Attempt, quality, ts),
	}
}

// floatPoint builds a FLOAT DataPoint.
func floatPoint(key string, value float64, quality qds.Quality, ts time.Time) *telemetry.DataPoint {
	return &telemetry.DataPoint{
		Key:       key,
		Type:      telemetry.DataPointType_FLOAT,
		Value:     float32(value),
		Quality:   qualityToProto(quality),
		Timestamp: timestamppb.New(ts),
	}
}

// intPoint builds an INTEGER (2-bit) DataPoint. Reserved for "<name>.position",
// the only tag with true double-point semantics (four states: Intermediate/
// Open/Closed/Bad, mirroring a real breaker's 52a/52b auxiliary contacts);
// every other discrete/status tag uses diPoint instead (see its comment).
func intPoint(key string, value int, quality qds.Quality, ts time.Time) *telemetry.DataPoint {
	return &telemetry.DataPoint{
		Key:       key,
		Type:      telemetry.DataPointType_INTEGER,
		Value:     float32(value),
		Quality:   qualityToProto(quality),
		Timestamp: timestamppb.New(ts),
	}
}

// diPoint builds a single-bit discrete-indication DataPoint: BOOLEAN type
// (1 bit) like boolPoint, but carrying a numeric value rather than
// true/false — for a multi-valued status (protection/autoreclose state,
// active settings group, reclose attempt count) that this simulator still
// classifies as ordinary 1-bit DI rather than double-point position's
// 2-bit "integer" (see intPoint's comment and pkg/breaker/README.md's
// "SCADA tag naming" section).
func diPoint(key string, value int, quality qds.Quality, ts time.Time) *telemetry.DataPoint {
	return &telemetry.DataPoint{
		Key:       key,
		Type:      telemetry.DataPointType_BOOLEAN,
		Value:     float32(value),
		Quality:   qualityToProto(quality),
		Timestamp: timestamppb.New(ts),
	}
}

// boolPoint builds a BOOLEAN DataPoint.
func boolPoint(key string, value bool, quality qds.Quality, ts time.Time) *telemetry.DataPoint {
	v := float32(0)
	if value {
		v = 1
	}
	return &telemetry.DataPoint{
		Key:       key,
		Type:      telemetry.DataPointType_BOOLEAN,
		Value:     v,
		Quality:   qualityToProto(quality),
		Timestamp: timestamppb.New(ts),
	}
}

// qualityToProto reduces a pkg/qds.Quality bitmask to the single
// DataPointQuality value telemetry.proto's DataPoint carries, in order of
// severity: an unusable reading (Invalid) outranks a merely uncertain one
// (Questionable), which outranks an operator override (Substituted), which
// outranks the two operational-state flags (Blocked, Test). This ordering
// is specific to this narrower proto enum — it is not pkg/qds.Quality's own
// GetPriority, which serves a different purpose (badge/border colouring
// over the full, wider set of quality bits).
func qualityToProto(q qds.Quality) telemetry.DataPointQuality {
	switch {
	case q.Has(qds.QdsInvalid):
		return telemetry.DataPointQuality_QDS_INVALID
	case q.Has(qds.QdsQuestionable):
		return telemetry.DataPointQuality_QDS_QUESTIONABLE
	case q.Has(qds.QdsSubstituted):
		return telemetry.DataPointQuality_QDS_SUBSTITUTED
	case q.Has(qds.QdsBlocked):
		return telemetry.DataPointQuality_QDS_BLOCKED
	case q.Has(qds.QdsTest):
		return telemetry.DataPointQuality_QDS_TEST
	default:
		return telemetry.DataPointQuality_QDS_GOOD
	}
}
