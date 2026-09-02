// Package faultsimserver implements the gRPC FaultSimulator service
// (api/scada/faultsim.proto) on top of one or more pkg/breaker.Simulator
// instances. Unlike internal/telemetryserver, this service mirrors no real
// DMS/SCADA backend contract — it exists only to let a test harness or
// operator drive pkg/breaker.EmulatorFeed against a running scada-simulator
// instance, simulating grid faults without modifying config or restarting
// the binary.
//
// A single InjectMeasurement call is not a one-shot event: it sets the
// named breaker's ongoing simulated analog condition, which this server
// then keeps re-feeding to EmulatorFeed.InjectMeasurement every
// sustainInterval — like a real plant feed continuously sampling the
// line — until a later InjectMeasurement call for the same breaker
// replaces it. A fault only clears when the caller explicitly sends a
// lower value (e.g. 0), exactly mirroring how a real fault stays on the
// line until it's actually cleared, not until some fixed timer expires.
package faultsimserver

import (
	"context"
	"sync"
	"time"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/faultsim"
	"scada-simulator/pkg/llog"
)

// sustainInterval is how often a breaker's last-injected measurement is
// re-fed to it — the simulated "sample rate" of an ongoing fault.
const sustainInterval = 200 * time.Millisecond

// FaultSimulatorServer implements faultsim.FaultSimulatorServer, feeding
// InjectMeasurement requests to the named breaker's
// pkg/breaker.EmulatorFeed and sustaining the most recent one — see the
// package doc comment.
type FaultSimulatorServer struct {
	// UnimplementedFaultSimulatorServer satisfies the forward-compatibility
	// requirement of faultsim.FaultSimulatorServer.
	faultsim.UnimplementedFaultSimulatorServer

	ctx      context.Context               // governs every sustain goroutine's lifetime; cancelled on shutdown
	breakers map[string]*breaker.Simulator // configured breakers, keyed by Config.Name
	logger   *llog.LevelLog                // logger for injected-fault activity

	sustainMu     sync.Mutex                    // guards sustainCancel
	sustainCancel map[string]context.CancelFunc // per-breaker cancel for its current sustain goroutine, keyed by Config.Name
}

// NewFaultSimulatorServer builds a FaultSimulatorServer over breakers,
// keyed by each Simulator's configured name. ctx should be the
// application's overall lifetime context — the same one breakers were
// themselves constructed with — since that is what stops every breaker's
// sustain goroutine when the application shuts down.
func NewFaultSimulatorServer(ctx context.Context, breakers map[string]*breaker.Simulator, logger *llog.LevelLog) *FaultSimulatorServer {
	return &FaultSimulatorServer{
		ctx:           ctx,
		breakers:      breakers,
		logger:        logger,
		sustainCancel: make(map[string]context.CancelFunc),
	}
}

// InjectMeasurement implements faultsim.FaultSimulatorServer, converting
// req into a breaker.Measurement, feeding it to the named breaker
// immediately via EmulatorFeed.InjectMeasurement, and then sustaining it
// (see sustain) so it keeps being the breaker's simulated ongoing analog
// condition until a later call replaces it. ERROR_BREAKER_NOT_FOUND is
// returned if Breaker doesn't name a configured breaker.
//
// Every call is logged at Info level twice: the incoming request (breaker
// name and every measurement field) before validation, and the outcome
// right before returning.
func (s *FaultSimulatorServer) InjectMeasurement(_ context.Context, req *faultsim.InjectMeasurementRequest) (*faultsim.InjectMeasurementResponse, error) {
	s.logger.Infof("faultsim: inject_measurement %s current=%v voltage=%v active_power=%.2f reactive_power=%.2f apparent_power=%.2f frequency=%.2f",
		req.GetBreaker(), req.GetCurrent(), req.GetVoltage(), req.GetActivePower(), req.GetReactivePower(), req.GetApparentPower(), req.GetFrequency())

	sim, ok := s.breakers[req.GetBreaker()]
	if !ok {
		s.logger.Warnf("faultsim: inject_measurement %s rejected: breaker not found", req.GetBreaker())
		return injectMeasurementResponse(faultsim.InjectMeasurementResult_ERROR_BREAKER_NOT_FOUND), nil
	}

	m := breaker.Measurement{
		Current:       phaseValues(req.GetCurrent()),
		Voltage:       phaseValues(req.GetVoltage()),
		ActivePower:   req.GetActivePower(),
		ReactivePower: req.GetReactivePower(),
		ApparentPower: req.GetApparentPower(),
		Frequency:     req.GetFrequency(),
	}
	s.sustain(req.GetBreaker(), sim, m)

	s.logger.Infof("faultsim: inject_measurement %s result=%s", req.GetBreaker(), faultsim.InjectMeasurementResult_OK)
	return injectMeasurementResponse(faultsim.InjectMeasurementResult_OK), nil
}

// sustain injects m into sim immediately, then starts a goroutine that
// keeps re-injecting it (with a fresh Timestamp each time, as a real
// periodic sample would carry) every sustainInterval — replacing any
// sustain goroutine name already has running, so at most one measurement
// is ever being sustained per breaker at a time. The new goroutine stops
// when a later call to sustain for name cancels it, or when s.ctx is done.
func (s *FaultSimulatorServer) sustain(name string, sim *breaker.Simulator, m breaker.Measurement) {
	s.sustainMu.Lock()
	if cancel, ok := s.sustainCancel[name]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.sustainCancel[name] = cancel
	s.sustainMu.Unlock()

	m.Timestamp = time.Now()
	_ = sim.InjectMeasurement(m) // always returns nil; see EmulatorFeed.InjectMeasurement

	go func() {
		ticker := time.NewTicker(sustainInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				tick := m
				tick.Timestamp = time.Now()
				_ = sim.InjectMeasurement(tick)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// phaseValues converts a faultsim.PhaseValues to a breaker.PhaseValues,
// treating a nil p (an unset field) as all-zero.
func phaseValues(p *faultsim.PhaseValues) breaker.PhaseValues {
	return breaker.PhaseValues{A: p.GetA(), B: p.GetB(), C: p.GetC()}
}

// injectMeasurementResponse wraps a result in an InjectMeasurementResponse.
func injectMeasurementResponse(result faultsim.InjectMeasurementResult) *faultsim.InjectMeasurementResponse {
	return &faultsim.InjectMeasurementResponse{Result: result}
}
