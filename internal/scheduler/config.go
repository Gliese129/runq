package scheduler

import "time"

// DiskConfig groups disk-freeze related tuning. These values are exposed to
// the SDK via RUNQ_* env vars — the daemon itself doesn't enforce them; the
// SDK uses them to compute NeededBytes for self-freeze decisions.
//
// `AutoThawCheckInterval` is the only purely-daemon-side field — it controls
// how often the scheduler polls disk usage on frozen mounts to issue
// SIGCONTs for tasks that can safely resume.
type DiskConfig struct {
	// SafetyFactorPercent is multiplied into upcoming-ckpt-size to compute
	// the free-bytes threshold needed for a save. 110 = 10% headroom over
	// the actual ckpt size.
	SafetyFactorPercent int `yaml:"safety_factor_percent"`

	// SafetyExtraGB is an additional absolute buffer (gigabytes) added on
	// top of the percentage. Useful for filesystems where small writes
	// (logs, tmp files) might land alongside the ckpt and starve it.
	SafetyExtraGB int `yaml:"safety_extra_gb"`

	// AutoThawCheckInterval is the auto-thaw poll cadence.
	AutoThawCheckInterval time.Duration `yaml:"auto_thaw_check_interval"`
}

// Config holds scheduler tuning parameters.
type Config struct {
	AgingThreshold     time.Duration // how long head-of-queue waits before reservation mode
	BackfillEnabled    bool
	TickInterval       time.Duration // how often the scheduler loop runs
	GPURefreshInterval time.Duration // how often to scan for external GPU usage (0 = disabled)

	// Phase 2F: how often to scan task dirs for orphans (0 = disabled).
	OrphanScanInterval time.Duration

	// L2-C: disk-freeze tuning. autoThawLoop runs when freeze != nil (no
	// separate enable flag — the auto-thaw goroutine is part of the
	// freeze feature, not an opt-in extra).
	Disk DiskConfig
}

// DefaultConfig returns sensible defaults for a research lab.
func DefaultConfig() Config {
	return Config{
		AgingThreshold:     15 * time.Minute,
		BackfillEnabled:    true,
		TickInterval:       1 * time.Second,
		GPURefreshInterval: 30 * time.Second,
		OrphanScanInterval: 5 * time.Minute,
		Disk: DiskConfig{
			SafetyFactorPercent:   110, // 10% headroom over raw ckpt size
			SafetyExtraGB:         0,   // no extra absolute buffer by default
			AutoThawCheckInterval: 60 * time.Second,
		},
	}
}
