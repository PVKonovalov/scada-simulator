package configuration

import (
	"path/filepath"
	"testing"
	"time"

	"scada-simulator/pkg/configuration"
)

// TestRead_SampleConfig checks the sample config/scada-simulator.yaml decodes
// both breaker.Config's own fields and the rtdb/rtdb_mapping extensions.
func TestRead_SampleConfig(t *testing.T) {
	var c Configuration
	if err := configuration.Read(filepath.Join("..", "..", "config", "scada-simulator.yaml"), &c); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if c.Rtdb.Port != 5010 || c.Rtdb.ReconnectInterval != 5*time.Second {
		t.Errorf("rtdb = %+v, want port 5010 and reconnect_interval 5s", c.Rtdb)
	}
	if len(c.Breakers) != 2 {
		t.Fatalf("len(breakers) = %d, want 2", len(c.Breakers))
	}
	tests := []struct {
		name    string
		idx     int
		breaker string
		mapping map[string]string
	}{
		{"feeder-1 mapped", 0, "feeder-1", map[string]string{
			"current.a": "case-1-branch-35-i-from",
			"current.b": "case-1-branch-35-i-from",
			"current.c": "case-1-branch-35-i-from",
		}},
		{"feeder-2 unmapped", 1, "feeder-2", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := c.Breakers[tt.idx]
			if b.Name != tt.breaker {
				t.Errorf("name = %q, want %q", b.Name, tt.breaker)
			}
			if b.MechanicalOperateTime != 60*time.Millisecond {
				t.Errorf("mechanical_operate_time = %s, want 60ms (breaker.Config fields lost)", b.MechanicalOperateTime)
			}
			if len(b.RtdbMapping) != len(tt.mapping) {
				t.Fatalf("rtdb_mapping = %v, want %v", b.RtdbMapping, tt.mapping)
			}
			for k, v := range tt.mapping {
				if b.RtdbMapping[k] != v {
					t.Errorf("rtdb_mapping[%q] = %q, want %q", k, b.RtdbMapping[k], v)
				}
			}
		})
	}
}

// TestRead_RtdbEnvOverride checks rtdb's flat host/port get their own
// SCADA_SIMULATOR_RTDB_* env vars.
func TestRead_RtdbEnvOverride(t *testing.T) {
	configuration.EnvVarPrefix = "SCADA_SIMULATOR_"
	t.Setenv("SCADA_SIMULATOR_RTDB_HOST", "rtdb.example")
	t.Setenv("SCADA_SIMULATOR_RTDB_PORT", "6000")
	var c Configuration
	if err := configuration.Read(filepath.Join("..", "..", "config", "scada-simulator.yaml"), &c); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if c.Rtdb.Host != "rtdb.example" || c.Rtdb.Port != 6000 {
		t.Errorf("rtdb host/port = %s:%d, want rtdb.example:6000", c.Rtdb.Host, c.Rtdb.Port)
	}
	if c.Dms.Api.Scada.Grpc.Port != 50051 {
		t.Errorf("grpc port = %d, want 50051 unaffected", c.Dms.Api.Scada.Grpc.Port)
	}
}
