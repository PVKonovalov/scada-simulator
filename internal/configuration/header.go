package configuration

import (
	"time"

	"scada-simulator/pkg/breaker"

	"gopkg.in/yaml.v3"
)

// Logging configures the application's leveled logger.
type Logging struct {
	// Level is the minimum log level to emit (trace/debug/info/warning/error).
	Level string `yaml:"level" env:"true"`
}

// Configuration is the application's top-level, YAML- and env-var-loaded
// configuration.
type Configuration struct {
	// Logging configures the application's leveled logger.
	Logging Logging `yaml:"logging"`
	// Dms configures the connection to the DMS (Distribution Management System).
	Dms struct {
		// Api configures the DMS's SCADA-facing API.
		Api struct {
			// Database configures the DMS database connection.
			Database struct {
				// Dsn is the Postgres connection string.
				Dsn string `yaml:"dsn" env:"true"`
			} `yaml:"database"`
			// Scada configures the endpoints this simulator serves as the
			// "SCADA" side of the DMS connection: a REST API (Host/Port)
			// for POST /api/token, and a gRPC API (Grpc.Host/Grpc.Port)
			// for the TelemetryStream service. Username/Password are the
			// credentials a DMS-side client (e.g. rdss-dms-rtdb) must
			// present to /api/token to authenticate.
			Scada struct {
				// Host is the REST API listen address.
				Host string `yaml:"host" env:"true"`
				// Port is the REST API listen port.
				Port int `yaml:"port" env:"true"`
				// Username a client must present to POST /api/token.
				Username string `yaml:"username" env:"true"`
				// Password a client must present to POST /api/token.
				Password string `yaml:"password" env:"true"`
				// Grpc configures the gRPC listener/endpoint.
				Grpc struct {
					// Host is the gRPC listen address.
					Host string `yaml:"host" env:"true"`
					// Port is the gRPC listen port.
					Port int `yaml:"port" env:"true"`
				} `yaml:"grpc"`
			} `yaml:"scada"`
		} `yaml:"api"`
	} `yaml:"dms"`
	// Breakers configures the simulated HV circuit breakers and protection
	// relays to emulate (see pkg/breaker.Config) — one entry per breaker,
	// each with its own name, initial position, protection settings,
	// autoreclose behaviour, and mechanical/frequency constants. The number
	// of breakers emulated is simply len(Breakers).
	Breakers []BreakerConfig `yaml:"breakers"`
	// Rtdb configures the optional, read-only RTDB measurement feed (see
	// internal/rtdbfeed) that drives breakers' analog inputs from RTDB
	// points named by each breaker's BreakerConfig.RtdbMapping.
	Rtdb Rtdb `yaml:"rtdb"`
}

// Rtdb configures the gRPC connection to the RTDB (rdss-dms-rtdb) that
// internal/rtdbfeed subscribes to. The feed only ever reads from the RTDB
// (IsExists/Subscribe) — it never writes to it; the SCADA→RTDB direction
// stays with rdss-dms-rtdb's rtdb-scada-grpc-client bridge. Host/Port are
// kept flat (not nested under a "grpc" key) so their env-var overrides are
// SCADA_SIMULATOR_RTDB_HOST/_PORT rather than colliding with
// Dms.Api.Scada.Grpc's SCADA_SIMULATOR_GRPC_HOST/_PORT.
type Rtdb struct {
	// Enabled turns the RTDB feed on; when false every RtdbMapping is ignored.
	Enabled bool `yaml:"enabled" env:"true"`
	// Host is the RTDB gRPC server's host.
	Host string `yaml:"host" env:"true"`
	// Port is the RTDB gRPC server's port.
	Port int `yaml:"port" env:"true"`
	// ClientId identifies this simulator in the RTDB's logs.
	ClientId string `yaml:"client_id" env:"true"`
	// ReconnectInterval is how long to wait between connection attempts,
	// both during initial startup and after an established stream breaks
	// (YAML duration string, e.g. "5s"; YAML-only, no env override).
	ReconnectInterval time.Duration `yaml:"reconnect_interval"`
}

// BreakerConfig is one breakers: list entry: a pkg/breaker.Config plus
// this application's own per-breaker extensions, kept out of pkg/breaker
// so that package stays unaware of the RTDB.
type BreakerConfig struct {
	// Config is the breaker simulator's own configuration.
	breaker.Config
	// RtdbMapping maps an internal analog tag leaf (the part of the tag
	// after "<breaker-name>.", e.g. "current.a" or "power.active") to the
	// RTDB point key whose value feeds it, e.g.
	// "current.a": "case-1-branch-35-i-from". Several leaves may share one
	// RTDB key. Only used when Rtdb.Enabled is true.
	RtdbMapping map[string]string `yaml:"rtdb_mapping"`
}

// UnmarshalYAML decodes a BreakerConfig. breaker.Config has its own
// UnmarshalYAML, which would otherwise be promoted to BreakerConfig and
// silently drop rtdb_mapping, so both halves are decoded from the same
// node explicitly.
func (c *BreakerConfig) UnmarshalYAML(value *yaml.Node) error {
	if err := value.Decode(&c.Config); err != nil {
		return err
	}
	var ext struct {
		RtdbMapping map[string]string `yaml:"rtdb_mapping"`
	}
	if err := value.Decode(&ext); err != nil {
		return err
	}
	c.RtdbMapping = ext.RtdbMapping
	return nil
}
