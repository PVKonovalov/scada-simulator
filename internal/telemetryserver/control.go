package telemetryserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/qds"
	"scada-simulator/pkg/telemetry"
)

// controlTagSuffix is appended to a breaker's name to form its control tag.
// It is deliberately distinct from "<name>.position" — see the "SCADA tag
// naming" section of pkg/breaker/README.md: a control point is a different
// SCADA object from the status point reporting the same physical breaker's
// position, even though both concern the same breaker.
const controlTagSuffix = ".control"

// modeTagSuffix is appended to a breaker's name to form its mode tag — the
// same tag Subscribe reports IEC 61850 Mod-style mode on (see
// dataPointsForEvent's breaker.ModeChangedDetail handling).
const modeTagSuffix = ".mode"

// SupervisoryControl implements telemetry.TelemetryStreamServer, performing
// telecontrol on a breaker's position.
//
// Key must be "<breaker-name>.control". Value is the target position:
// 0 = Open, 1 = Close; any other value is rejected. Select/Execute follow
// pkg/breaker.BreakerController's select-before-operate semantics: a
// select-only request arms the command and remembers the resulting
// SelectionID (there is no field on this message for a caller to echo one
// back), and a later execute-only request for the same Key carries it out.
// A request with both Select and Execute set performs both in one call,
// with no separate select phase.
//
// Test is a pure quality modifier, layered on top of whatever Select/
// Execute already request — it does not change what action is taken
// (Select-only still only arms; a request with neither Select nor Execute
// still does nothing). Setting it marks the breaker as being in test mode:
// every tag this server reports for it via Subscribe carries qds.QdsTest
// quality until a later SupervisoryControl call for the same breaker
// explicitly sets Test back to false, which clears it (this call's own
// status log line below is unaffected — it always reports the simulator's
// real, unmarked quality, for accurate diagnostics). Test mode is recorded
// unconditionally from every call's Test value (see setTestMode below),
// independent of whether that call's command itself succeeds.
//
// Every call is logged at Info level twice: the incoming request (key,
// value, flags) before any validation, and the outcome — result, whether
// the breaker's position actually changed (moved), and its position/
// quality/protection status at that moment — right before returning.
// moved answers "did the switch flip" directly, so a caller does not have
// to infer it by watching whether "<name>.position" passed through
// Intermediate on Subscribe: it is true only when Operate actually started
// a mechanical transition (Position reads Intermediate at this point, not
// yet settled — see beginOperate/completeOperate). moved is always false
// for a Select-only call, or one with neither Select nor Execute (neither
// ever moves the breaker on its own), and also false for an Operate that
// found the breaker already at the requested position (see
// EventPositionChanged's doc comment on that still-successful, still-
// confirmed case) — in both of the latter cases, result is still OK, so a
// caller must check both fields, not result alone, to know whether
// anything physically happened.
func (s *TelemetryServer) SupervisoryControl(ctx context.Context, req *telemetry.ScadaSupervisoryControlRequest) (*telemetry.ScadaSupervisoryControlResponse, error) {
	s.logger.Infof("telemetry: control %s value=%d select=%t execute=%t test=%t client_id=%q",
		req.GetKey(), req.GetValue(), req.GetSelect(), req.GetExecute(), req.GetTest(), req.GetClientId())

	name, ok := strings.CutSuffix(req.GetKey(), controlTagSuffix)
	if !ok {
		return controlResponse(telemetry.ScadaSupervisoryControlResult_ERROR_ITEM_IS_NOT_FOUND), nil
	}
	sim, ok := s.breakers[name]
	if !ok {
		return controlResponse(telemetry.ScadaSupervisoryControlResult_ERROR_OBJECT_IS_NOT_FOUND), nil
	}
	s.setTestMode(name, req.GetTest())

	target, ok := positionForControlValue(req.GetValue())
	if !ok {
		return controlResponse(telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SUPPORTED), nil
	}
	cmd := breaker.Command{Target: target, Source: req.GetClientId(), Timestamp: time.Now()}

	beforePos, blockQuality := sim.Position()

	var result telemetry.ScadaSupervisoryControlResult
	if blockQuality.Has(qds.QdsBlocked) {
		s.logger.Warnf("telemetry: control %s rejected: breaker is blocked", req.GetKey())
		result = telemetry.ScadaSupervisoryControlResult_ERROR_BLOCKED
	} else {
		switch {
		case req.GetSelect() && req.GetExecute():
			result = s.selectAndExecute(ctx, sim, cmd)
		case req.GetSelect():
			result = s.selectControl(ctx, req.GetKey(), sim, cmd)
		case req.GetExecute():
			result = s.executeControl(ctx, req.GetKey(), sim, cmd)
		default:
			// Neither Select nor Execute was set: nothing to do (Test alone
			// cannot act — see the method doc comment).
			result = telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SUPPORTED
		}
	}

	pos, posQuality := sim.Position()
	ps := sim.ProtectionStatus()
	s.logger.Infof("telemetry: control %s result=%s moved=%t breaker position=%s (was %s) quality=%s protection=%s",
		req.GetKey(), result, pos != beforePos, pos, beforePos, posQuality, ps.State)
	return controlResponse(result), nil
}

// ProtectionTerminalSetMode implements telemetry.TelemetryStreamServer,
// changing a breaker's IEC 61850 Mod-style operating mode.
//
// Key must be "<breaker-name>.mode". Mode is mapped to breaker.Mode via
// modeForProto (ERROR_NOT_SUPPORTED if it isn't one of
// ProtectionTerminalMode's four modelled values) and passed to
// BreakerController.SetMode along with Reason. The resulting mode is
// enforced by SupervisoryControl (ERROR_BLOCKED while Blocked or Off) and
// reflected on Subscribe via the same "<name>.mode" tag, exactly as if
// SetMode had been called directly on the Simulator.
//
// Every call is logged at Info level twice, matching SupervisoryControl:
// the incoming request before validation, and the outcome right before
// returning.
func (s *TelemetryServer) ProtectionTerminalSetMode(ctx context.Context, req *telemetry.ProtectionTerminalSetModeRequest) (*telemetry.ProtectionTerminalSetModeResponse, error) {
	s.logger.Infof("telemetry: set_mode %s mode=%s reason=%q client_id=%q",
		req.GetKey(), req.GetMode(), req.GetReason(), req.GetClientId())

	name, ok := strings.CutSuffix(req.GetKey(), modeTagSuffix)
	if !ok {
		return setModeResponse(telemetry.ScadaSupervisoryControlResult_ERROR_ITEM_IS_NOT_FOUND), nil
	}
	sim, ok := s.breakers[name]
	if !ok {
		return setModeResponse(telemetry.ScadaSupervisoryControlResult_ERROR_OBJECT_IS_NOT_FOUND), nil
	}
	mode, ok := modeForProto(req.GetMode())
	if !ok {
		return setModeResponse(telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SUPPORTED), nil
	}

	result := telemetry.ScadaSupervisoryControlResult_OK
	if err := sim.SetMode(ctx, mode, req.GetReason()); err != nil {
		result = controlErrorResult(err)
	}

	s.logger.Infof("telemetry: set_mode %s result=%s mode=%s", req.GetKey(), result, sim.Mode())
	return setModeResponse(result), nil
}

// setModeResponse wraps a result in a ProtectionTerminalSetModeResponse.
func setModeResponse(result telemetry.ScadaSupervisoryControlResult) *telemetry.ProtectionTerminalSetModeResponse {
	return &telemetry.ProtectionTerminalSetModeResponse{Result: result}
}

// modeForProto maps a ProtectionTerminalMode to a breaker.Mode, reporting
// ok=false for MODE_UNKNOWN or any value this simulator doesn't model (e.g.
// IEC 61850's TEST/BLOCKED, 4).
func modeForProto(m telemetry.ProtectionTerminalMode) (breaker.Mode, bool) {
	switch m {
	case telemetry.ProtectionTerminalMode_MODE_ON:
		return breaker.ModeOn, true
	case telemetry.ProtectionTerminalMode_MODE_BLOCKED:
		return breaker.ModeBlocked, true
	case telemetry.ProtectionTerminalMode_MODE_TEST:
		return breaker.ModeTest, true
	case telemetry.ProtectionTerminalMode_MODE_OFF:
		return breaker.ModeOff, true
	default:
		return 0, false
	}
}

// selectAndExecute performs Select immediately followed by Operate, for a
// client that wants a one-shot command with no separate select phase (also
// used for Test — see SupervisoryControl's doc comment).
func (s *TelemetryServer) selectAndExecute(ctx context.Context, sim *breaker.Simulator, cmd breaker.Command) telemetry.ScadaSupervisoryControlResult {
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		return controlErrorResult(err)
	}
	if err := sim.Operate(ctx, id, cmd); err != nil {
		return controlErrorResult(err)
	}
	return telemetry.ScadaSupervisoryControlResult_OK
}

// selectControl arms cmd and remembers its SelectionID under key for a
// later executeControl call.
func (s *TelemetryServer) selectControl(ctx context.Context, key string, sim *breaker.Simulator, cmd breaker.Command) telemetry.ScadaSupervisoryControlResult {
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		return controlErrorResult(err)
	}
	s.pendingMu.Lock()
	s.pending[key] = id
	s.pendingMu.Unlock()
	return telemetry.ScadaSupervisoryControlResult_OK
}

// executeControl carries out the selection previously armed by
// selectControl under key.
func (s *TelemetryServer) executeControl(ctx context.Context, key string, sim *breaker.Simulator, cmd breaker.Command) telemetry.ScadaSupervisoryControlResult {
	s.pendingMu.Lock()
	id, ok := s.pending[key]
	delete(s.pending, key)
	s.pendingMu.Unlock()
	if !ok {
		// Execute with no matching prior Select for key — including one
		// that already expired or was consumed by an earlier Execute.
		return telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SELECTED
	}

	err := sim.Operate(ctx, id, cmd)
	if err == nil {
		return telemetry.ScadaSupervisoryControlResult_OK
	}
	if errors.Is(err, breaker.ErrInvalidCommand) {
		// Simulator leaves a target-mismatched selection armed; put it back
		// so the caller can retry with a corrected Value.
		s.pendingMu.Lock()
		s.pending[key] = id
		s.pendingMu.Unlock()
	}
	return controlErrorResult(err)
}

// positionForControlValue maps a control command's Value to a target
// Position: 0 = Open, 1 = Close.
func positionForControlValue(value int64) (breaker.Position, bool) {
	switch value {
	case 0:
		return breaker.PositionOpen, true
	case 1:
		return breaker.PositionClosed, true
	default:
		return 0, false
	}
}

// controlErrorResult maps an error from BreakerController to the closest
// ScadaSupervisoryControlResult. The proto's result enum is coarse, so
// several distinct pkg/breaker errors necessarily collapse onto
// ERROR_UNKNOW.
func controlErrorResult(err error) telemetry.ScadaSupervisoryControlResult {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return telemetry.ScadaSupervisoryControlResult_ERROR_TIMEOUT
	case isType[*breaker.SelectionExpiredError](err):
		return telemetry.ScadaSupervisoryControlResult_ERROR_TIMEOUT
	case errors.Is(err, breaker.ErrInvalidCommand):
		return telemetry.ScadaSupervisoryControlResult_ERROR_NOT_SUPPORTED
	default:
		// *breaker.InterlockError (already-blocked is handled before
		// reaching here) and *breaker.UnknownSelectionError have no closer
		// match in this proto's result enum.
		return telemetry.ScadaSupervisoryControlResult_ERROR_UNKNOW
	}
}

// isType reports whether err's tree contains an error of type T.
func isType[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

// controlResponse wraps a result in a ScadaSupervisoryControlResponse.
func controlResponse(result telemetry.ScadaSupervisoryControlResult) *telemetry.ScadaSupervisoryControlResponse {
	return &telemetry.ScadaSupervisoryControlResponse{Result: result}
}
