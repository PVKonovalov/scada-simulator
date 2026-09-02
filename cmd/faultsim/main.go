// Command faultsim is a console client for api/scada/faultsim.proto's
// FaultSimulator gRPC service: it injects one three-phase measurement into
// a named breaker of a running scada-simulator instance, letting an
// operator or script trigger a real trip/autoreclose sequence without
// touching the server's config or restarting it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"scada-simulator/pkg/faultsim"
)

// phaseFlag parses a "-current"/"-voltage" flag value: either a single
// number applied to all three phases (the common symmetric three-phase
// fault case) or three comma-separated numbers "a,b,c" for an asymmetric
// fault (e.g. a single-phase-to-ground fault).
func phaseFlag(s string) (*faultsim.PhaseValues, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	values := make([]float64, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q: %w", p, err)
		}
		values[i] = v
	}
	switch len(values) {
	case 1:
		return &faultsim.PhaseValues{A: values[0], B: values[0], C: values[0]}, nil
	case 3:
		return &faultsim.PhaseValues{A: values[0], B: values[1], C: values[2]}, nil
	default:
		return nil, fmt.Errorf("want either one value or three comma-separated values (a,b,c), got %d", len(values))
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "scada-simulator gRPC server address")
	breakerName := flag.String("breaker", "", "breaker name to inject into, e.g. feeder-1 (required)")
	current := flag.String("current", "", "phase current(s), amps RMS: a single value for all three phases, or \"a,b,c\"")
	voltage := flag.String("voltage", "", "phase voltage(s), volts RMS phase-to-neutral: a single value for all three phases, or \"a,b,c\"")
	activePower := flag.Float64("active-power", 0, "three-phase active power, W")
	reactivePower := flag.Float64("reactive-power", 0, "three-phase reactive power, VAR")
	apparentPower := flag.Float64("apparent-power", 0, "three-phase apparent power, VA")
	frequency := flag.Float64("frequency", 0, "power-system frequency, Hz")
	timeout := flag.Duration("timeout", 5*time.Second, "gRPC call timeout")
	flag.Parse()

	if *breakerName == "" {
		fmt.Fprintln(os.Stderr, "missing required -breaker flag")
		flag.Usage()
		os.Exit(2)
	}

	currentValues, err := phaseFlag(*current)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-current: %v\n", err)
		os.Exit(2)
	}
	voltageValues, err := phaseFlag(*voltage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-voltage: %v\n", err)
		os.Exit(2)
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to %s: %v\n", *addr, err)
		os.Exit(1)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := faultsim.NewFaultSimulatorClient(conn)
	resp, err := client.InjectMeasurement(ctx, &faultsim.InjectMeasurementRequest{
		Breaker:       *breakerName,
		Current:       currentValues,
		Voltage:       voltageValues,
		ActivePower:   *activePower,
		ReactivePower: *reactivePower,
		ApparentPower: *apparentPower,
		Frequency:     *frequency,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "InjectMeasurement: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(resp.GetResult())
	if resp.GetResult() != faultsim.InjectMeasurementResult_OK {
		os.Exit(1)
	}
}
