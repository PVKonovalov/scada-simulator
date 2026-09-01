package configuration

import "scada-simulator/pkg/breaker"

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
	Breakers []breaker.Config `yaml:"breakers"`
}
