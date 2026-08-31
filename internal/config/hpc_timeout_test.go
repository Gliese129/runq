package config

import (
	"strings"
	"testing"
)

func TestRunqPresetIncludesTimeoutPlaceholder(t *testing.T) {
	cfg, err := HPCPreset("runq")
	if err != nil {
		t.Fatalf("HPCPreset(runq): %v", err)
	}
	if !strings.Contains(cfg.SubmitTemplate, "--timeout {{timeout}}") {
		t.Fatalf("runq submit template does not forward timeout: %s", cfg.SubmitTemplate)
	}

	for _, result := range cfg.CheckHPC() {
		if result.Name != "submit_template" {
			continue
		}
		if result.Status != "ok" {
			t.Fatalf("submit template check failed: %+v", result)
		}
		if !strings.Contains(result.Detail, "--timeout '60'") {
			t.Fatalf("submit template preview does not render timeout: %s", result.Detail)
		}
		return
	}
	t.Fatal("submit template check result not found")
}
