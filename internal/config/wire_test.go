package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTargetConfigJSONWireShape pins the /api/v1 wire contract: every
// TargetConfig field the dashboard consumes must serialize as snake_case.
// Regression for RQ-67 review finding (fields with yaml-only tags leaked
// PascalCase onto the wire, breaking the Settings target editor).
func TestTargetConfigJSONWireShape(t *testing.T) {
	cfg := TargetConfig{
		Name:      "hpc-a",
		GPUs:      []int{0, 1},
		Scheduler: "slurm",
		Workspace: "/scratch/u",
		SSH:       &SSHTargetConfig{Host: "login1", User: "u", Key: "/k", Port: 22, ProxyJump: "jump"},

		MaxInflight:    4,
		SubmitTemplate: "sbatch {{run_sh}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "scancel {{ext_id}}",
		StatusParser:   []string{"head -1"},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)

	wantKeys := []string{
		`"name"`, `"gpus"`, `"scheduler"`, `"workspace"`, `"ssh"`,
		`"host"`, `"user"`, `"key"`, `"port"`, `"proxy_jump"`,
		`"max_inflight"`, `"submit_template"`, `"submit_id_regex"`,
		`"kill_template"`, `"status_parser"`,
	}
	for _, k := range wantKeys {
		if !strings.Contains(got, k) {
			t.Errorf("wire JSON missing %s: %s", k, got)
		}
	}
	// Any exported-Go-style key on the wire means a json tag is missing.
	for _, bad := range []string{`"Name"`, `"GPUs"`, `"Scheduler"`, `"Workspace"`, `"SSH"`, `"Host"`, `"ProxyJump"`} {
		if strings.Contains(got, bad) {
			t.Errorf("wire JSON leaked PascalCase key %s: %s", bad, got)
		}
	}

	// Round trip: the dashboard PUTs snake_case back — it must decode.
	var back TargetConfig
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Name != cfg.Name || back.Scheduler != cfg.Scheduler || back.SSH == nil || back.SSH.Host != cfg.SSH.Host {
		t.Errorf("round trip lost fields: %+v", back)
	}
}
