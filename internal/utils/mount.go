package utils

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/shirou/gopsutil/v4/disk"
)

// LoadMountTable enumerates the active physical partitions via gopsutil.
// `false` skips virtual filesystems (proc, overlay, tmpfs) — callers care
// about disk mounts, not kernel ones.
//
// Cost is one statfs-equivalent per mountpoint. On lab machines with a
// handful of disks this is sub-millisecond. On NFS-heavy setups it can
// take 10-100 ms; that's why callers in hot paths (scheduler.tick) should
// load the table once per cycle and pass the result to MountOf, NOT call
// LoadMountTable per task.
func LoadMountTable() ([]disk.PartitionStat, error) {
	return disk.Partitions(false)
}

// MountOf returns the mountpoint that `path` lives under, by longest-prefix
// match against `parts`. EvalSymlinks is applied so soft links resolve to
// the real mount — `/home/user/ckpts → /data/foo` would otherwise match
// the wrong partition.
//
// Returns "" when the path can't be resolved or no partition matches.
// Callers should treat that as "unknown mount" — skipping the task is
// usually right; never group multiple unknowns under "" because they may
// actually be on different unmounted overlays.
//
// `parts` is typically obtained from LoadMountTable() and reused across a
// hot loop. Cheap to call — O(N) over the partition list with no syscalls.
func MountOf(path string, parts []disk.PartitionStat) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	// EvalSymlinks may fail if the path doesn't exist yet (e.g. a
	// CheckpointDir that the task hasn't created); fall back to the
	// absolute path in that case. The matching below still works as long
	// as some ancestor of the path exists and matches a mount.
	if real_, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real_
	}

	best := ""
	sep := string(os.PathSeparator)
	for _, p := range parts {
		// Match if path == mountpoint exactly, or path lives under it.
		// The +sep guards against "/data" matching "/data2".
		if p.Mountpoint == abs || strings.HasPrefix(abs, p.Mountpoint+sep) {
			if len(p.Mountpoint) > len(best) {
				best = p.Mountpoint
			}
		}
	}
	return best
}
