package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	config "scada-simulator/internal/configuration"
	"scada-simulator/internal/faultsimserver"
	flags2 "scada-simulator/internal/flags"
	"scada-simulator/internal/restserver"
	"scada-simulator/internal/telemetryserver"
	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/configuration"
	"scada-simulator/pkg/faultsim"
	"scada-simulator/pkg/llog"
	"scada-simulator/pkg/telemetry"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// gracefulStopTimeout bounds how long the gRPC server waits for open
// streams (e.g. Subscribe) to close on their own during shutdown before
// forcing every connection closed.
const gracefulStopTimeout = 5 * time.Second

func main() {
	configuration.EnvVarPrefix = "SCADA_SIMULATOR_"

	flags := flags2.NewFlags()
	if flags.Parse() {
		return
	}

	if flags.PathToConfig == "" {
		llog.Logger.Errorf("Missing required -config flag: path to yaml configuration file")
		return
	}

	if err := configuration.Read(flags.PathToConfig, config.Config); err != nil {
		llog.Logger.Errorf("Failed to read configuration (%s): %v", flags.PathToConfig, err)
		return
	}

	if logLevel, err := llog.ParseLevel(config.Config.Logging.Level); err != nil {
		llog.Logger.Errorf("Failed to parse log level (%s): %v", config.Config.Logging.Level, err)
		llog.Logger.SetLevel(llog.DebugLevel)
	} else {
		llog.Logger.SetLevel(logLevel)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if len(config.Config.Breakers) == 0 {
		llog.Logger.Fatalf("No breakers configured: config.breakers is empty")
	}

	simulators := make(map[string]*breaker.Simulator, len(config.Config.Breakers))
	for _, cfg := range config.Config.Breakers {
		if _, exists := simulators[cfg.Name]; exists {
			llog.Logger.Fatalf("Duplicate breaker name in config: %q", cfg.Name)
		}
		simulators[cfg.Name] = breaker.New(ctx, cfg.WithConfig(llog.Logger))
	}
	defer func() {
		for _, sim := range simulators {
			sim.Close()
		}
	}()
	llog.Logger.Infof("%d breaker(s) started", len(simulators))
	for _, cfg := range config.Config.Breakers {
		sim := simulators[cfg.Name]
		pos, quality := sim.Position()
		ps := sim.ProtectionStatus()
		ar := sim.AutoRecloseStatus()
		llog.Logger.Infof("breaker %q: position=%s quality=%s protection=%s autoreclose=%s",
			cfg.Name, pos, quality, ps.State, ar.State)
	}

	// TelemetryStream's Subscribe and SupervisoryControl are both
	// implemented by telemetryserver.TelemetryServer, backed by simulators.
	gRpcServer := grpc.NewServer()
	telemetry.RegisterTelemetryStreamServer(gRpcServer, telemetryserver.NewTelemetryServer(ctx, simulators, llog.Logger))

	// FaultSimulator is served alongside TelemetryStream on the same gRPC
	// server/port — see api/scada/faultsim.proto's package doc for why it's
	// a separate service rather than an addition to TelemetryStream.
	faultsim.RegisterFaultSimulatorServer(gRpcServer, faultsimserver.NewFaultSimulatorServer(ctx, simulators, llog.Logger))

	if llog.Logger.GetLevel() >= llog.DebugLevel {
		reflection.Register(gRpcServer)
	}

	bind := fmt.Sprintf("%s:%d", config.Config.Dms.Api.Scada.Grpc.Host, config.Config.Dms.Api.Scada.Grpc.Port)
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		llog.Logger.Fatalf("failed to listen on %s: %v Exiting.", bind, err)
	}

	// The REST API only serves POST /api/token today (see internal/restserver's
	// package doc) — the login every REST/gRPC client is expected to call
	// first, per api/scada/telemetry.proto's Subscribe doc comment.
	restBind := fmt.Sprintf("%s:%d", config.Config.Dms.Api.Scada.Host, config.Config.Dms.Api.Scada.Port)
	httpServer := &http.Server{
		Addr:    restBind,
		Handler: restserver.NewServer(config.Config.Dms.Api.Scada.Username, config.Config.Dms.Api.Scada.Password, simulators, llog.Logger).Handler(),
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if srvErr := gRpcServer.Serve(listener); srvErr != nil {
			llog.Logger.Fatalf("gRPC server failed to listen: %v", srvErr)
			return
		}
		llog.Logger.Infof("gRPC service stopped")
	})
	wg.Go(func() {
		if srvErr := httpServer.ListenAndServe(); srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
			llog.Logger.Fatalf("REST server failed to listen: %v", srvErr)
			return
		}
		llog.Logger.Infof("REST service stopped")
	})

	llog.Logger.Infof("scada-simulator started, gRPC listen %s, REST listen %s", listener.Addr(), restBind)
	<-ctx.Done()
	llog.Logger.Infof("shutting down")

	// GracefulStop waits for every open stream (e.g. Subscribe) to close on
	// its own, which never happens for a client that stays connected - so
	// force-close everything after a grace period. httpServer.Shutdown has
	// the same "wait, then give up" shape via its own context deadline.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), gracefulStopTimeout)
	defer stopCancel()
	if err := httpServer.Shutdown(stopCtx); err != nil {
		llog.Logger.Warnf("REST server did not shut down cleanly: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		gRpcServer.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(gracefulStopTimeout):
		llog.Logger.Warnf("Graceful stop timed out with clients still connected, forcing shutdown")
		gRpcServer.Stop()
	}

	wg.Wait()
}
