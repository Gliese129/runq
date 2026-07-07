package utils

import (
	"os"
	"path/filepath"
)

// RunningMode indicates how the daemon data directory was resolved.
type RunningMode int

const (
	ModeRoot   RunningMode = iota // euid == 0, using /var/lib/runq
	ModeUser                      // regular user, using ~/.local/share/runq
	ModeCustom                    // explicit override via flag or env
)

// ResolveDataDir determines the daemon data directory.
// Priority: RUNQ_DATA_DIR env > root mode (/var/lib/runq) > user mode (~/.local/share/runq).
// All daemon files (DB, socket, PID, logs) live under this directory.
func ResolveDataDir() (RunningMode, string) {
	if dir := os.Getenv("RUNQ_DATA_DIR"); dir != "" {
		return ModeCustom, dir
	}
	if os.Geteuid() == 0 {
		return ModeRoot, "/var/lib/runq"
	}
	home, _ := os.UserHomeDir()
	return ModeUser, filepath.Join(home, ".local", "share", "runq")
}

// DataDirPaths returns all standard paths derived from a data directory.
type DataDirPaths struct {
	DataDir    string
	DBPath     string
	SocketPath string
	PIDPath    string
	LogDir     string
}

// PathsFromDataDir computes the CLIENT daemon's file paths from a data
// directory. The client inherits every legacy filename (runq.db / runq.sock /
// daemon.pid) on purpose: pre-split job history stays visible in the
// dashboard and existing CLI socket paths keep working.
func PathsFromDataDir(dataDir string) DataDirPaths {
	return DataDirPaths{
		DataDir:    dataDir,
		DBPath:     filepath.Join(dataDir, "runq.db"),
		SocketPath: filepath.Join(dataDir, "runq.sock"),
		PIDPath:    filepath.Join(dataDir, "daemon.pid"),
		LogDir:     filepath.Join(dataDir, "logs"),
	}
}

// RunqdPathsFromDataDir computes the EXECUTION daemon's (runqd) file paths.
// Same data root, distinct names: runqd owns its own store (the execution
// ledger) and its own socket — the two-daemon split means two single-writer
// stores, never two writers on one file.
func RunqdPathsFromDataDir(dataDir string) DataDirPaths {
	return DataDirPaths{
		DataDir:    dataDir,
		DBPath:     filepath.Join(dataDir, "runqd.db"),
		SocketPath: filepath.Join(dataDir, "runqd.sock"),
		PIDPath:    filepath.Join(dataDir, "runqd.pid"),
		LogDir:     filepath.Join(dataDir, "logs"),
	}
}
