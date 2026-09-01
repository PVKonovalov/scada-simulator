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
			// Scada configures the SCADA gRPC endpoint the DMS exposes.
			Scada struct {
				// Username authenticates the simulator to the DMS.
				Username string `yaml:"username" env:"true"`
				// Password authenticates the simulator to the DMS.
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
	// Breaker configures the simulated HV circuit breaker and protection
	// relay (see pkg/breaker.Config) — its initial position, protection
	// settings, autoreclose behaviour, and mechanical/frequency constants.
	Breaker breaker.Config `yaml:"breaker"`
}
