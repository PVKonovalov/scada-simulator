// Package telemetryserver implements the gRPC TelemetryStream service
// (api/scada/telemetry.proto) on top of one or more pkg/breaker.Simulator
// instances.
package telemetryserver

import (
	"context"
	"strings"
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
// command changes no tag. EventModeChanged is the only source of
// "<name>.mode" updates (see dataPointsForEvent) — SetMode also emits
// EventPositionChanged, but that alone must not push ".mode" too, or every
// ordinary position change (which also carries a Quality) would resend an
// unchanged ".mode" value.
var liveEventKinds = []breaker.EventKind{
	breaker.EventPositionChanged,
	breaker.EventMeasurementChanged,
	breaker.EventProtectionPickedUp,
	breaker.EventTrip,
	breaker.EventAutoRecloseAttempt,
	breaker.EventLockout,
	breaker.EventAutoRecloseSucceeded,
	breaker.EventSettingsChanged,
	breaker.EventModeChanged,
}

// tripProtectionTagSuffix, autoRecloseFailedTagSuffix and
// autoRecloseSucceededTagSuffix name the one-shot pulse tags pushed by
// dataPointsForEvent alongside its usual statusPoints refresh — see that
// function's doc comment. They carry no persistent value and are never part
// of breakerSnapshot; TagsFor lists them explicitly so they can still be
// provisioned ahead of time, the same way it does for controlTagSuffix.
const (
	tripProtectionTagSuffix       = ".trip.protection"
	autoRecloseFailedTagSuffix    = ".autoreclose.false"
	autoRecloseSucceededTagSuffix = ".autoreclose.true"
)

// positionTagSuffix names "<name>.position". Like the pulse tags above, it
// is exempt from dedupeForBroadcast: EventPositionChanged deliberately
// fires (see its doc comment in pkg/breaker/events.go) even when Operate is
// a no-op, purely to confirm a command a client just issued — silently
// dropping that confirmation because the position value didn't change would
// defeat the entire point of sending it.
const positionTagSuffix = ".position"

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

	lastMu sync.Mutex           // guards last
	last   map[string]lastPoint // last (value, quality) forward actually broadcast for each non-pulse tag key; see dedupeForBroadcast
}

// lastPoint is the (value, quality) pair last broadcast for one tag key, as
// last actually sent it — see dedupeForBroadcast.
type lastPoint struct {
	value   float32
	quality telemetry.DataPointQuality
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
		last:        make(map[string]lastPoint),
	}
	for name, sim := range breakers {
		s.seedLast(breakerSnapshot(name, sim, false))
		events, err := sim.Subscribe(ctx, liveEventKinds...)
		if err != nil {
			continue // breaker already closed; nothing to forward for it
		}
		go s.forward(name, sim, events)
	}
	return s
}

// seedLast primes dedupeForBroadcast's cache with points' current (value,
// quality), so the very first real event forwarded for a breaker is
// filtered against its actual starting state (e.g. voltage/power sitting at
// their startup default of 0) rather than an empty cache that would let
// every one of that first event's tags through unconditionally.
func (s *TelemetryServer) seedLast(points []*telemetry.DataPoint) {
	s.lastMu.Lock()
	defer s.lastMu.Unlock()
	for _, p := range points {
		s.last[p.GetKey()] = lastPoint{value: p.GetValue(), quality: p.GetQuality()}
	}
}

// forward is the single, long-lived event-to-tag converter for one
// breaker; see TelemetryServer's doc comment.
func (s *TelemetryServer) forward(name string, sim *breaker.Simulator, events <-chan breaker.Event) {
	for ev := range events {
		points := s.dedupeForBroadcast(dataPointsForEvent(name, sim, ev, s.isTestMode(name)))
		if len(points) == 0 {
			continue
		}
		update := &telemetry.SubstationUpdate{Points: points}
		s.logUpdate("update", update)
		s.broadcast(update)
	}
}

// dedupeForBroadcast drops every point whose (value, quality) exactly
// matches the last one this server actually broadcast for its key, so a
// breaker.Event that re-reads unchanged state (e.g. dataPointsForEvent's
// statusPoints re-read alongside an unrelated tag's real change, or a
// sustained fault injection re-feeding a measurement whose current changed
// but whose voltage/power/frequency didn't — see
// pkg/breaker.measurementChanged) never causes a duplicate push: the
// simulator must not publish a tag unless its value or quality actually
// changed. alwaysForward tags (the pulse tags, and "<name>.position" for its
// idempotent-confirmation case) are exempt.
func (s *TelemetryServer) dedupeForBroadcast(points []*telemetry.DataPoint) []*telemetry.DataPoint {
	if len(points) == 0 {
		return points
	}

	s.lastMu.Lock()
	defer s.lastMu.Unlock()

	kept := points[:0]
	for _, p := range points {
		if alwaysForward(p.GetKey()) {
			kept = append(kept, p)
			continue
		}
		cur := lastPoint{value: p.GetValue(), quality: p.GetQuality()}
		if prev, ok := s.last[p.GetKey()]; ok && prev == cur {
			continue
		}
		s.last[p.GetKey()] = cur
		kept = append(kept, p)
	}
	return kept
}

// alwaysForward reports whether key must bypass dedupeForBroadcast: the
// one-shot pulse tags (dataPointsForEvent's doc comment — they carry no
// persistent value, so every occurrence must be forwarded even if
// numerically identical to the last one) and "<name>.position" (see
// positionTagSuffix's doc comment — its idempotent-confirmation push must
// reach the client even when the position didn't change).
func alwaysForward(key string) bool {
	return strings.HasSuffix(key, tripProtectionTagSuffix) ||
		strings.HasSuffix(key, autoRecloseFailedTagSuffix) ||
		strings.HasSuffix(key, autoRecloseSucceededTagSuffix) ||
		strings.HasSuffix(key, positionTagSuffix)
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
		intPoint(name+".mode", int(sim.Mode()), posQuality, now),
	}
	points = append(points, measurementPoints(name, sim.Measurement(), testMode)...)
	points = append(points, statusPoints(name, sim, now, testMode)...)
	return points
}

// dataPointsForEvent maps one breaker.Event to the tags it should refresh,
// or nil if the event carries no tag update of its own. testMode marks
// every tag qds.QdsTest when true — see applyTestMode. Note this only
// refreshes "<name>.position" itself on a real position change —
// "<name>.mode" is refreshed solely by EventModeChanged (via sim.Position()'s
// quality, since it carries no quality of its own), even though SetMode
// also emits EventPositionChanged: an ordinary position change is not a
// mode change, and must not resend an unchanged ".mode" value.
//
// EventTrip/EventLockout/EventAutoRecloseSucceeded additionally push a
// one-shot pulse tag (tripProtectionTagSuffix/autoRecloseFailedTagSuffix/
// autoRecloseSucceededTagSuffix, value always true) alongside their
// statusPoints refresh: unlike every other tag here, a pulse carries no
// persistent value of its own — it just marks that the event happened —
// so it is deliberately never part of breakerSnapshot's initial snapshot.
func dataPointsForEvent(name string, sim *breaker.Simulator, ev breaker.Event, testMode bool) []*telemetry.DataPoint {
	switch d := ev.Detail.(type) {
	case breaker.PositionChangedDetail:
		return []*telemetry.DataPoint{
			intPoint(name+".position", int(d.Position), applyTestMode(d.Quality, testMode), ev.Timestamp),
		}
	case breaker.Measurement:
		return measurementPoints(name, d, testMode)
	case breaker.ModeChangedDetail:
		_, quality := sim.Position()
		quality = applyTestMode(quality, testMode)
		return []*telemetry.DataPoint{intPoint(name+".mode", int(d.Mode), quality, ev.Timestamp)}
	case breaker.TripDetail:
		return append(statusPoints(name, sim, ev.Timestamp, testMode),
			pulsePoint(name+tripProtectionTagSuffix, testMode, ev.Timestamp))
	case breaker.LockoutDetail:
		return append(statusPoints(name, sim, ev.Timestamp, testMode),
			pulsePoint(name+autoRecloseFailedTagSuffix, testMode, ev.Timestamp))
	case breaker.AutoRecloseSucceededDetail:
		return append(statusPoints(name, sim, ev.Timestamp, testMode),
			pulsePoint(name+autoRecloseSucceededTagSuffix, testMode, ev.Timestamp))
	default:
		// EventProtectionPickedUp/AutoRecloseAttempt/SettingsChanged: rather
		// than hand-decode each Detail type, just re-read the current
		// protection/autoreclose status — cheap, and always exactly right.
		return statusPoints(name, sim, ev.Timestamp, testMode)
	}
}

// pulsePoint builds a one-shot PROTECTION_EVENT pulse tag (always value 1 —
// see dataPointsForEvent), distinct from every ordinary status/analog tag's
// FLOAT/INTEGER/BOOLEAN typing precisely because it carries no persistent
// value of its own, only an occurrence. Carries qds.QdsGood quality, plus
// qds.QdsTest when testMode is true.
func pulsePoint(key string, testMode bool, ts time.Time) *telemetry.DataPoint {
	return &telemetry.DataPoint{
		Key:       key,
		Type:      telemetry.DataPointType_PROTECTION_EVENT,
		Value:     1,
		Quality:   qualityToProto(applyTestMode(qds.QdsGood, testMode)),
		Timestamp: timestamppb.New(ts),
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

// intPoint builds an INTEGER DataPoint: "<name>.position" (2-bit double-point,
// four states — Intermediate/Open/Closed/Bad, mirroring a real breaker's
// 52a/52b auxiliary contacts) and "<name>.mode" (breaker.Mode's five values,
// which don't fit a 1-bit boolean/DI) are this simulator's only tags typed
// this way; every other discrete/status tag uses diPoint instead (see its
// comment).
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
// classifies as ordinary 1-bit DI rather than an "integer" tag (see
// intPoint's comment and pkg/breaker/README.md's "SCADA tag naming"
// section).
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
	case q.Has(qds.QdsBlocked) && q.Has(qds.QdsTest):
		// pkg/breaker.ModeTestBlocked sets both bits at once — checked
		// before the singular cases below so it isn't reduced to plain
		// QDS_BLOCKED.
		return telemetry.DataPointQuality_QDS_TEST_BLOCKED
	case q.Has(qds.QdsBlocked):
		return telemetry.DataPointQuality_QDS_BLOCKED
	case q.Has(qds.QdsTest):
		return telemetry.DataPointQuality_QDS_TEST
	default:
		return telemetry.DataPointQuality_QDS_GOOD
	}
}
