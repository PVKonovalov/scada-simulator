package flags

import (
	"flag"
	"fmt"
	config "scada-simulator/internal/configuration"
	"scada-simulator/pkg/configuration"
)

type Flags struct {
	PathToConfig string
	ShowEnvVars  bool
}

func NewFlags() *Flags {
	return &Flags{}
}

// Parse command line flags and return true if the program should exit after showing env vars
func (f *Flags) Parse() bool {
	flag.StringVar(&f.PathToConfig, "config", "", "path to yaml configuration file")
	flag.BoolVar(&f.ShowEnvVars, "env", false, "show a list of configuration options that can be loaded from the environment")

	flag.Parse()

	if f.ShowEnvVars {
		fmt.Printf("%+v\n", configuration.ListEnv(config.Config))
		return true
	}
	return false
}
