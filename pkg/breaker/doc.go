// Package breaker models a high-voltage (HV) circuit breaker and its
// protection relay (IED), exposing a SCADA-facing interface, plus a
// runnable in-memory [Simulator] that drives the model with injected analog
// values via [EmulatorFeed].
//
// This is a domain simulation for engineering and demonstration purposes,
// not a real protocol stack. It deliberately does NOT implement IEC 61850,
// IEC 60870-5-104, DNP3, Modbus, OPC UA, or any other wire protocol — the
// types in scada.go are pure Go interfaces and values; a real SCADA gateway
// would implement protocol adapters on top of them. It also does not model
// arc physics, waveform/sample-level behavior, or contact wear/aging.
//
// All state is held in memory and lost on process exit — there is no
// persistence. Control operations carry no authentication or authorization;
// Command.Source is informational only.
package breaker
