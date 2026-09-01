package breaker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"scada-simulator/pkg/qds"
)

// Compile-time checks that Simulator satisfies every SCADA-facing interface.
var (
	_ PositionReader          = (*Simulator)(nil)
	_ AnalogReader            = (*Simulator)(nil)
	_ ProtectionConfigurator  = (*Simulator)(nil)
	_ AutoRecloseConfigurator = (*Simulator)(nil)
	_ BreakerController       = (*Simulator)(nil)
	_ ProtectionStatusReader  = (*Simulator)(nil)
	_ EmulatorFeed            = (*Simulator)(nil)
	_ EventSubscriber         = (*Simulator)(nil)
)

// subscriberBuffer is how many events an EventSubscriber channel buffers
// before the oldest queued event is dropped in favor of the newest.
const subscriberBuffer = 32

// selection is a pending select-before-operate reservation.
type selection struct {
	cmd Command // the Selected command; Operate's cmd.Target must match it
}

// subscriber is one EventSubscriber registration.
type subscriber struct {
	ch    chan Event         // the channel handed back to the caller
	kinds map[EventKind]bool // kinds this subscriber wants; empty means all kinds
}

// wants reports whether the subscriber wants events of kind k.
func (s *subscriber) wants(k EventKind) bool {
	if len(s.kinds) == 0 {
		return true
	}
	return s.kinds[k]
}

// simState is every piece of mutable state a Simulator owns. It is only
// ever touched from within the Simulator's run loop goroutine — external
// callers reach it exclusively through Simulator.do/doCtx closures, which
// is what keeps the whole simulator race-free without a mutex.
type simState struct {
	name string // this breaker's name, used to prefix log messages

	position    Position    // current breaker position
	posQuality  qds.Quality // quality attached to position; kept in sync with blocked via qds.QdsBlocked
	measurement Measurement // most recently injected measurement
	lastEvalAt  time.Time   // wall-clock time of the previous InjectMeasurement, for stage-51 integration

	blocked     bool   // true when Block has been called and Unblock has not yet cleared it; the source of truth behind qds.QdsBlocked on posQuality
	blockReason string // reason passed to Block, surfaced on any control command rejected while blocked

	settings    [NumSettingsGroups]ProtectionSettings // stored protection settings, one per group
	activeGroup SettingsGroup                         // currently active settings group

	protectionState ProtectionState // overall protection relay state
	stage51Progress float64         // fraction (0..1) of the stage-51 operate time consumed so far
	instArmed       bool            // whether the stage-50 instantaneous timer is currently armed

	autoReclose *autoReclose // the autoreclose sequencing state machine
	arClosing   bool         // true while the in-flight Close operation was issued by autoReclose, not a SCADA operator

	mechGen int // incremented on every beginOperate, so a stale mechanical-completion timer can recognise it has been superseded

	selections        map[SelectionID]*selection // active select-before-operate reservations
	expiredSelections map[SelectionID]struct{}   // recently expired selection ids, kept briefly so Operate/Cancel can report SelectionExpiredError
	subscribers       map[int]*subscriber        // active EventSubscriber registrations, keyed by an internal id
	nextSubID         int                        // next id to hand out in subscribers

	mechOperateTime  time.Duration // configured mechanical operate time
	selectionTimeout time.Duration // configured selection timeout
	nominalFrequency float64       // configured nominal power-system frequency, Hz

	logger Logger // internal structured logger
}

// Simulator is the concrete in-memory implementation of every SCADA-facing
// interface in this package, plus EmulatorFeed and EventSubscriber. A
// single goroutine started by New owns simState and serializes all reads,
// writes, timers and control commands through it; see simState's comment.
type Simulator struct {
	cmdCh  chan func(*simState) // commands the run loop executes against simState, in order
	ctx    context.Context      // governs the run loop's lifetime
	cancel context.CancelFunc   // stops the run loop
	doneCh chan struct{}        // closed once the run loop has returned

	st simState // owned exclusively by the run loop goroutine; see simState
}

// New constructs and starts a Simulator. The returned Simulator's run loop
// goroutine runs until ctx is cancelled or Close is called. Zero-valued
// tunable fields in cfg take the defaults documented on Config.
func New(ctx context.Context, cfg Config) *Simulator {
	cfg = cfg.withDefaults()

	runCtx, cancel := context.WithCancel(ctx)
	s := &Simulator{
		cmdCh:  make(chan func(*simState), 8),
		ctx:    runCtx,
		cancel: cancel,
		doneCh: make(chan struct{}),
	}

	var settings [NumSettingsGroups]ProtectionSettings
	for _, g := range cfg.SettingsGroups {
		if int(g.Group) < NumSettingsGroups {
			settings[g.Group] = g.Settings
		}
	}

	s.st = simState{
		name:              cfg.Name,
		position:          cfg.InitialPosition,
		posQuality:        qds.QdsGood,
		settings:          settings,
		activeGroup:       cfg.ActiveSettingsGroup,
		protectionState:   ProtectionNormal,
		autoReclose:       newAutoReclose(cfg.AutoReclose),
		selections:        make(map[SelectionID]*selection),
		expiredSelections: make(map[SelectionID]struct{}),
		subscribers:       make(map[int]*subscriber),
		mechOperateTime:   cfg.MechanicalOperateTime,
		selectionTimeout:  cfg.SelectionTimeout,
		nominalFrequency:  cfg.NominalFrequencyHz,
		logger:            cfg.Logger,
	}

	go s.run()
	return s
}

// Close stops the run loop and waits for it to exit. It is safe to call
// more than once.
func (s *Simulator) Close() error {
	s.cancel()
	<-s.doneCh
	return nil
}

// run is the Simulator's single run loop goroutine: it owns simState and
// executes every queued command against it in order, until ctx is
// cancelled.
func (s *Simulator) run() {
	defer close(s.doneCh)
	defer func() {
		for id, sub := range s.st.subscribers {
			close(sub.ch)
			delete(s.st.subscribers, id)
		}
	}()

	for {
		select {
		case fn := <-s.cmdCh:
			fn(&s.st)
		case <-s.ctx.Done():
			return
		}
	}
}

// post queues fn to run on the loop goroutine without waiting for it to
// execute; it is a no-op if the loop has already stopped.
func (s *Simulator) post(fn func(*simState)) {
	select {
	case s.cmdCh <- fn:
	case <-s.ctx.Done():
	}
}

// do queues fn and blocks until the loop goroutine has executed it (or the
// loop has stopped, in which case fn may never run).
func (s *Simulator) do(fn func(*simState)) {
	done := make(chan struct{})
	select {
	case s.cmdCh <- func(st *simState) { fn(st); close(done) }:
	case <-s.ctx.Done():
		return
	}
	select {
	case <-done:
	case <-s.ctx.Done():
	}
}

// ErrClosed is returned by ctx-aware operations issued after the simulator
// has been closed.
var ErrClosed = errClosed{}

// errClosed is the concrete type behind ErrClosed.
type errClosed struct{}

// Error implements the error interface.
func (errClosed) Error() string { return "breaker: simulator closed" }

// doCtx queues fn and blocks until the loop goroutine has executed it, ctx
// is cancelled, or the simulator is closed — whichever comes first.
func (s *Simulator) doCtx(ctx context.Context, fn func(*simState)) error {
	done := make(chan struct{})
	wrapped := func(st *simState) { fn(st); close(done) }
	select {
	case s.cmdCh <- wrapped:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return ErrClosed
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return ErrClosed
	}
}

// after schedules fn to run on the loop goroutine once d has elapsed.
func (s *Simulator) after(d time.Duration, fn func(*simState)) {
	time.AfterFunc(d, func() { s.post(fn) })
}

// emit publishes an event of the given kind/detail to every subscriber
// that wants it, and logs notable kinds via st.logger.
func (s *Simulator) emit(st *simState, kind EventKind, detail any) {
	ev := Event{Kind: kind, Timestamp: time.Now(), Detail: detail}
	for _, sub := range st.subscribers {
		if sub.wants(kind) {
			trySend(sub.ch, ev)
		}
	}

	switch kind {
	case EventTrip:
		st.logger.Warnf("breaker[%s]: protection trip: %+v", st.name, detail)
	case EventLockout:
		st.logger.Errorf("breaker[%s]: autoreclose lockout: %+v", st.name, detail)
	case EventAutoRecloseAttempt:
		st.logger.Infof("breaker[%s]: autoreclose attempt: %+v", st.name, detail)
	case EventControlRejected:
		st.logger.Warnf("breaker[%s]: control rejected: %+v", st.name, detail)
	case EventSettingsChanged:
		st.logger.Infof("breaker[%s]: settings changed: %+v", st.name, detail)
	}
}

// trySend delivers ev to ch without blocking, dropping the oldest queued
// event to make room if ch is full. ch has a single writer (the run loop),
// so this drop-oldest sequence cannot race with another sender.
func trySend(ch chan Event, ev Event) {
	select {
	case ch <- ev:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- ev:
	default:
	}
}

// newSelectionID generates a random, opaque SelectionID.
func newSelectionID() SelectionID {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return SelectionID(hex.EncodeToString(b[:]))
}

// --- PositionReader / AnalogReader / ProtectionStatusReader -------------

// Position implements PositionReader.
func (s *Simulator) Position() (Position, qds.Quality) {
	var p Position
	var q qds.Quality
	s.do(func(st *simState) { p, q = st.position, st.posQuality })
	return p, q
}

// Measurement implements AnalogReader.
func (s *Simulator) Measurement() Measurement {
	var m Measurement
	s.do(func(st *simState) { m = st.measurement })
	return m
}

// ProtectionStatus implements ProtectionStatusReader.
func (s *Simulator) ProtectionStatus() ProtectionStatus {
	var ps ProtectionStatus
	s.do(func(st *simState) {
		ps = ProtectionStatus{State: st.protectionState, ActiveGroup: st.activeGroup}
	})
	return ps
}

// AutoRecloseStatus implements ProtectionStatusReader.
func (s *Simulator) AutoRecloseStatus() AutoRecloseStatus {
	var status AutoRecloseStatus
	s.do(func(st *simState) { status = st.autoReclose.Status() })
	return status
}

// --- ProtectionConfigurator ----------------------------------------------

// ActiveSettingsGroup implements ProtectionConfigurator.
func (s *Simulator) ActiveSettingsGroup() SettingsGroup {
	var g SettingsGroup
	s.do(func(st *simState) { g = st.activeGroup })
	return g
}

// ProtectionSettings implements ProtectionConfigurator.
func (s *Simulator) ProtectionSettings(group SettingsGroup) (ProtectionSettings, error) {
	if err := validSettingsGroup(group); err != nil {
		return ProtectionSettings{}, err
	}
	var ps ProtectionSettings
	s.do(func(st *simState) { ps = st.settings[group] })
	return ps, nil
}

// SetProtectionSettings implements ProtectionConfigurator.
func (s *Simulator) SetProtectionSettings(group SettingsGroup, settings ProtectionSettings) error {
	if err := validSettingsGroup(group); err != nil {
		return err
	}
	s.do(func(st *simState) {
		st.settings[group] = settings
		if group == st.activeGroup {
			st.stage51Progress = 0
			st.instArmed = false
		}
		s.emit(st, EventSettingsChanged, SettingsChangedDetail{Group: group, SettingsUpdated: true})
	})
	return nil
}

// SelectSettingsGroup implements ProtectionConfigurator.
func (s *Simulator) SelectSettingsGroup(group SettingsGroup) error {
	if err := validSettingsGroup(group); err != nil {
		return err
	}
	s.do(func(st *simState) {
		st.activeGroup = group
		st.stage51Progress = 0
		st.instArmed = false
		s.emit(st, EventSettingsChanged, SettingsChangedDetail{Group: group, GroupSelected: true})
	})
	return nil
}

// --- AutoRecloseConfigurator ----------------------------------------------

// AutoRecloseConfig implements AutoRecloseConfigurator.
func (s *Simulator) AutoRecloseConfig() AutoRecloseConfig {
	var cfg AutoRecloseConfig
	s.do(func(st *simState) { cfg = st.autoReclose.config })
	return cfg
}

// SetAutoRecloseConfig implements AutoRecloseConfigurator. It resets the
// sequence to AutoRecloseReady, so it should not be called while a reclose
// sequence is in flight without understanding that consequence.
func (s *Simulator) SetAutoRecloseConfig(cfg AutoRecloseConfig) error {
	if cfg.Enabled && len(cfg.DeadTimes) < cfg.MaxAttempts {
		return ErrInvalidCommand
	}
	s.do(func(st *simState) {
		st.autoReclose = newAutoReclose(cfg)
	})
	return nil
}

// --- EmulatorFeed ----------------------------------------------------------

// InjectMeasurement implements EmulatorFeed.
func (s *Simulator) InjectMeasurement(m Measurement) error {
	s.do(func(st *simState) { s.evaluateProtection(st, m) })
	return nil
}

// evaluateProtection stores m and runs one protection evaluation cycle
// against it: the instantaneous (50) stage is checked directly, and the
// time-overcurrent (51) stage's operate timer is integrated using the
// elapsed wall-clock time since the previous injected measurement — the
// same "percent of operate time consumed" model real numerical relays use,
// which needs no background timer of its own.
func (s *Simulator) evaluateProtection(st *simState, m Measurement) {
	st.measurement = m
	s.emit(st, EventMeasurementChanged, m)

	now := time.Now()
	var dt time.Duration
	if !st.lastEvalAt.IsZero() {
		dt = now.Sub(st.lastEvalAt)
	}
	st.lastEvalAt = now

	if st.protectionState == ProtectionTripped || st.protectionState == ProtectionLockout {
		return
	}

	current := m.Current.Max()
	settings := st.settings[st.activeGroup]

	s.evaluateInstantaneous(st, settings, current)
	s.evaluateTimeOvercurrent(st, settings, current, dt)
}

// evaluateInstantaneous arms the stage-50 instantaneous stage's fixed-delay
// timer the first time current exceeds InstantaneousPickup, and disarms it
// if current drops back below pickup before the timer fires.
func (s *Simulator) evaluateInstantaneous(st *simState, settings ProtectionSettings, current float64) {
	delay, ok := InstantaneousOperateTime(settings, current, st.nominalFrequency)
	if !ok {
		st.instArmed = false
		return
	}
	if st.instArmed {
		return
	}
	st.instArmed = true
	s.emit(st, EventProtectionPickedUp, PickedUpDetail{Cause: CauseInstantaneous, Current: current})
	s.after(delay, func(st *simState) {
		if !st.instArmed || st.protectionState == ProtectionTripped || st.protectionState == ProtectionLockout {
			return
		}
		st.instArmed = false
		s.trip(st, CauseInstantaneous, current, delay)
	})
}

// evaluateTimeOvercurrent integrates the stage-51 operate timer: while
// current exceeds pickup, stage51Progress advances by dt/OperateTime each
// call and trips at 1; while current is below pickup, it decays back
// toward 0 over ResetTime.
func (s *Simulator) evaluateTimeOvercurrent(st *simState, settings ProtectionSettings, current float64, dt time.Duration) {
	total, pickedUp := OperateTime(settings, current)
	if !pickedUp {
		if st.stage51Progress <= 0 {
			return
		}
		if settings.ResetTime > 0 {
			st.stage51Progress -= dt.Seconds() / settings.ResetTime.Seconds()
		} else {
			st.stage51Progress = 0
		}
		if st.stage51Progress <= 0 {
			st.stage51Progress = 0
			if st.protectionState == ProtectionPickedUp {
				st.protectionState = ProtectionNormal
			}
		}
		return
	}

	if st.stage51Progress <= 0 && st.protectionState == ProtectionNormal {
		st.protectionState = ProtectionPickedUp
		s.emit(st, EventProtectionPickedUp, PickedUpDetail{Cause: CauseTimeOvercurrent, Current: current})
	}
	if total > 0 {
		st.stage51Progress += dt.Seconds() / total.Seconds()
	}
	if st.stage51Progress >= 1 {
		s.trip(st, CauseTimeOvercurrent, current, total)
	}
}

// trip issues a protection trip: it emits EventTrip, opens the breaker (if
// not already open) and, once the breaker is confirmed open, hands off to
// onBreakerOpen to consider an autoreclose attempt.
func (s *Simulator) trip(st *simState, cause ProtectionCause, current float64, operateTime time.Duration) {
	st.protectionState = ProtectionTripped
	st.stage51Progress = 0
	st.instArmed = false
	s.emit(st, EventTrip, TripDetail{Cause: cause, Current: current, OperateTime: operateTime})

	if st.position != PositionOpen {
		s.beginOperate(st, PositionOpen, false)
		return
	}
	s.onBreakerOpen(st)
}

// onBreakerOpen runs once the breaker is confirmed Open following a trip.
// If autoreclose is enabled it starts (or advances) the reclose sequence;
// if the sequence has just entered Lockout, it reflects that onto
// protectionState and emits EventLockout.
func (s *Simulator) onBreakerOpen(st *simState) {
	if st.protectionState != ProtectionTripped || !st.autoReclose.config.Enabled {
		return
	}

	deadTime, willReclose := st.autoReclose.OnTrip()
	if willReclose {
		s.emit(st, EventAutoRecloseAttempt, AutoRecloseAttemptDetail{Attempt: st.autoReclose.attempt, DeadTime: deadTime})
		s.after(deadTime, func(st *simState) {
			if st.autoReclose.OnDeadTimeElapsed() {
				s.beginOperate(st, PositionClosed, true)
			}
		})
		return
	}

	if st.autoReclose.state == AutoRecloseLockout {
		st.protectionState = ProtectionLockout
		s.emit(st, EventLockout, LockoutDetail{Attempts: st.autoReclose.config.MaxAttempts})
	}
}

// beginOperate starts a mechanical Open or Close operation: Position()
// reports PositionIntermediate until mechOperateTime elapses, at which
// point completeOperate settles it at target. viaAutoReclose marks whether
// this operation was issued by the autoreclose sequence rather than a
// SCADA operator, so completeOperate knows whether to advance it.
func (s *Simulator) beginOperate(st *simState, target Position, viaAutoReclose bool) {
	st.mechGen++
	gen := st.mechGen
	st.position = PositionIntermediate
	st.arClosing = viaAutoReclose
	s.emit(st, EventPositionChanged, PositionChangedDetail{Position: st.position, Quality: st.posQuality})

	s.after(st.mechOperateTime, func(st *simState) {
		if st.mechGen != gen {
			return // superseded by a later operation
		}
		s.completeOperate(st, target)
	})
}

// completeOperate settles the breaker at target once its mechanical
// operate time has elapsed, and drives any follow-on protection/autoreclose
// bookkeeping that depends on reaching that position.
func (s *Simulator) completeOperate(st *simState, target Position) {
	st.position = target
	s.emit(st, EventPositionChanged, PositionChangedDetail{Position: st.position, Quality: st.posQuality})

	switch target {
	case PositionOpen:
		s.onBreakerOpen(st)
	case PositionClosed:
		if st.protectionState == ProtectionTripped {
			st.protectionState = ProtectionNormal
		}
		if st.arClosing {
			st.arClosing = false
			reclaim := st.autoReclose.OnCloseSucceeded()
			s.after(reclaim, func(st *simState) {
				st.autoReclose.OnReclaimElapsed()
			})
		}
	}
}

// --- BreakerController -----------------------------------------------------

// Select implements BreakerController.
func (s *Simulator) Select(ctx context.Context, cmd Command) (SelectionID, error) {
	var id SelectionID
	var opErr error
	err := s.doCtx(ctx, func(st *simState) { id, opErr = s.doSelect(st, cmd) })
	if err != nil {
		return "", err
	}
	return id, opErr
}

// doSelect is the run-loop body of Select.
func (s *Simulator) doSelect(st *simState, cmd Command) (SelectionID, error) {
	if cmd.Target != PositionOpen && cmd.Target != PositionClosed {
		return "", ErrInvalidCommand
	}
	if st.blocked {
		return "", s.rejectControl(st, cmd, "breaker is blocked: "+st.blockReason)
	}
	if len(st.selections) > 0 {
		return "", s.rejectControl(st, cmd, "a selection is already in progress")
	}
	if st.autoReclose.state == AutoRecloseDeadTime || st.autoReclose.state == AutoRecloseClosing {
		return "", s.rejectControl(st, cmd, "automatic reclose sequence in progress")
	}

	id := newSelectionID()
	st.selections[id] = &selection{cmd: cmd}
	timeout := st.selectionTimeout
	s.after(timeout, func(st *simState) { s.expireSelection(st, id) })
	return id, nil
}

// expiredSelectionTombstoneTTL is the minimum time an expired selection's
// id is remembered so a late Operate/Cancel can still report
// SelectionExpiredError instead of UnknownSelectionError. It floors
// selectionTimeout so a deployment configured with a very short
// SelectionTimeout still gives callers a reasonable window to notice the
// expiry and report it accurately.
const expiredSelectionTombstoneTTL = time.Second

// expireSelection frees a selection that was never Operated or Cancelled
// in time, keeping a tombstone so a late Operate/Cancel can report
// SelectionExpiredError instead of UnknownSelectionError.
func (s *Simulator) expireSelection(st *simState, id SelectionID) {
	if _, ok := st.selections[id]; !ok {
		return // already Operated or Cancelled
	}
	delete(st.selections, id)
	st.expiredSelections[id] = struct{}{}
	ttl := max(st.selectionTimeout, expiredSelectionTombstoneTTL)
	s.after(ttl, func(st *simState) { delete(st.expiredSelections, id) })
}

// Operate implements BreakerController.
func (s *Simulator) Operate(ctx context.Context, id SelectionID, cmd Command) error {
	var opErr error
	err := s.doCtx(ctx, func(st *simState) { opErr = s.doOperate(st, id, cmd) })
	if err != nil {
		return err
	}
	return opErr
}

// doOperate is the run-loop body of Operate.
func (s *Simulator) doOperate(st *simState, id SelectionID, cmd Command) error {
	sel, ok := st.selections[id]
	if !ok {
		if _, expired := st.expiredSelections[id]; expired {
			err := &SelectionExpiredError{ID: id}
			s.emit(st, EventControlRejected, ControlRejectedDetail{Command: cmd, Reason: err.Error()})
			return err
		}
		return &UnknownSelectionError{ID: id}
	}
	if cmd.Target != sel.cmd.Target {
		return ErrInvalidCommand
	}
	if st.blocked {
		// Leave the selection intact: it remains usable if Unblock is
		// called before it times out.
		return s.rejectControl(st, cmd, "breaker is blocked: "+st.blockReason)
	}
	delete(st.selections, id)

	if st.position == sel.cmd.Target && st.position != PositionIntermediate {
		// Idempotent: already at the requested position. Still emit a
		// confirmation so a SCADA client that just issued the command sees
		// a positive response on the tag, not silence — see
		// EventPositionChanged's doc comment.
		s.emit(st, EventPositionChanged, PositionChangedDetail{Position: st.position, Quality: st.posQuality})
		return nil
	}
	if st.position == PositionIntermediate {
		return s.rejectControl(st, cmd, "breaker mechanism already in motion")
	}

	s.beginOperate(st, sel.cmd.Target, false)
	return nil
}

// Cancel implements BreakerController.
func (s *Simulator) Cancel(ctx context.Context, id SelectionID) error {
	var opErr error
	err := s.doCtx(ctx, func(st *simState) { opErr = s.doCancel(st, id) })
	if err != nil {
		return err
	}
	return opErr
}

// doCancel is the run-loop body of Cancel.
func (s *Simulator) doCancel(st *simState, id SelectionID) error {
	if _, ok := st.selections[id]; ok {
		delete(st.selections, id)
		return nil
	}
	if _, ok := st.expiredSelections[id]; ok {
		delete(st.expiredSelections, id)
		return nil
	}
	return &UnknownSelectionError{ID: id}
}

// ResetLockout implements BreakerController.
func (s *Simulator) ResetLockout(ctx context.Context) error {
	var opErr error
	err := s.doCtx(ctx, func(st *simState) {
		if opErr = st.autoReclose.ResetLockout(); opErr != nil {
			return
		}
		st.protectionState = ProtectionNormal
		st.stage51Progress = 0
		st.instArmed = false
	})
	if err != nil {
		return err
	}
	return opErr
}

// Block implements BreakerController.
func (s *Simulator) Block(ctx context.Context, reason string) error {
	return s.doCtx(ctx, func(st *simState) {
		st.blocked = true
		st.blockReason = reason
		st.posQuality.Set(qds.QdsBlocked)
		s.emit(st, EventControlBlocked, ControlBlockedDetail{Reason: reason})
		s.emit(st, EventPositionChanged, PositionChangedDetail{Position: st.position, Quality: st.posQuality})
	})
}

// Unblock implements BreakerController.
func (s *Simulator) Unblock(ctx context.Context) error {
	return s.doCtx(ctx, func(st *simState) {
		st.blocked = false
		st.blockReason = ""
		st.posQuality.Clear(qds.QdsBlocked)
		s.emit(st, EventControlUnblocked, ControlUnblockedDetail{})
		s.emit(st, EventPositionChanged, PositionChangedDetail{Position: st.position, Quality: st.posQuality})
	})
}

// rejectControl builds and emits an InterlockError as an EventControlRejected.
func (s *Simulator) rejectControl(st *simState, cmd Command, reason string) error {
	err := &InterlockError{Reason: reason}
	s.emit(st, EventControlRejected, ControlRejectedDetail{Command: cmd, Reason: reason})
	return err
}

// --- EventSubscriber ---------------------------------------------------

// Subscribe implements EventSubscriber.
func (s *Simulator) Subscribe(ctx context.Context, kinds ...EventKind) (<-chan Event, error) {
	ch := make(chan Event, subscriberBuffer)
	kindSet := make(map[EventKind]bool, len(kinds))
	for _, k := range kinds {
		kindSet[k] = true
	}

	err := s.doCtx(ctx, func(st *simState) {
		id := st.nextSubID
		st.nextSubID++
		st.subscribers[id] = &subscriber{ch: ch, kinds: kindSet}

		go func() {
			select {
			case <-ctx.Done():
			case <-s.ctx.Done():
			}
			s.post(func(st *simState) {
				if sub, ok := st.subscribers[id]; ok {
					close(sub.ch)
					delete(st.subscribers, id)
				}
			})
		}()
	})
	if err != nil {
		close(ch)
		return nil, err
	}
	return ch, nil
}
