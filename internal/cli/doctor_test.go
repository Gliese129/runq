package cli

import (
	"testing"

	"github.com/gliese129/runq-lab/internal/config"
)

func TestHasExecutorRoleRequiresConfiguredLocalTarget(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.GlobalConfig
		want bool
	}{
		{name: "nil", cfg: nil, want: false},
		{name: "unconfigured", cfg: &config.GlobalConfig{}, want: false},
		{name: "remote only", cfg: &config.GlobalConfig{Targets: []config.TargetConfig{{Name: "hpc", Scheduler: "slurm"}}}, want: false},
		{name: "explicit local", cfg: &config.GlobalConfig{Targets: []config.TargetConfig{{Name: "lab-gpu"}}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasExecutorRole(tt.cfg); got != tt.want {
				t.Fatalf("hasExecutorRole() = %v, want %v", got, tt.want)
			}
		})
	}
}
