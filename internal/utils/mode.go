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

// PathsFromDataDir computes all daemon file paths from a data directory.
func PathsFromDataDir(dataDir string) DataDirPaths {
	return DataDirPaths{
		DataDir:    dataDir,
		DBPath:     filepath.Join(dataDir, "runq.db"),
		SocketPath: filepath.Join(dataDir, "runq.sock"),
		PIDPath:    filepath.Join(dataDir, "daemon.pid"),
		LogDir:     filepath.Join(dataDir, "logs"),
	}
}
