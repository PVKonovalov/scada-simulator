package main

import (
	"context"
	"fmt"
	"net"
	"os/signal"
	config "scada-simulator/internal/configuration"
	flags2 "scada-simulator/internal/flags"
	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/configuration"
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

	sim := breaker.New(ctx, config.Config.Breaker.WithConfig(llog.Logger))
	defer sim.Close()

	// gRpcServer has no service logic yet: TelemetryStream is registered
	// with its generated UnimplementedTelemetryStreamServer, so every RPC
	// currently reports codes.Unimplemented until a real implementation
	// (backed by sim) replaces it.
	gRpcServer := grpc.NewServer()
	telemetry.RegisterTelemetryStreamServer(gRpcServer, telemetry.UnimplementedTelemetryStreamServer{})

	if llog.Logger.GetLevel() >= llog.DebugLevel {
		reflection.Register(gRpcServer)
	}

	bind := fmt.Sprintf("%s:%d", config.Config.Dms.Api.Scada.Grpc.Host, config.Config.Dms.Api.Scada.Grpc.Port)
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		llog.Logger.Fatalf("failed to listen on %s: %v Exiting.", bind, err)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if srvErr := gRpcServer.Serve(listener); srvErr != nil {
			llog.Logger.Fatalf("gRPC server failed to listen: %v", srvErr)
			return
		}
		llog.Logger.Infof("gRPC service stopped")
	})

	llog.Logger.Infof("scada-simulator started, gRPC listen %s", listener.Addr())
	<-ctx.Done()
	llog.Logger.Infof("shutting down")

	// GracefulStop waits for every open stream (e.g. a future Subscribe
	// implementation) to close on its own, which never happens for a
	// client that stays connected - so force-close everything after a
	// grace period.
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
