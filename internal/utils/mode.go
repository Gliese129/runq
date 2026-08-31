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

// RunqdSocketPath resolves the independently installed execution daemon's
// socket using runqd's own precedence and defaults. runq-lab must not derive
// this from RUNQ_DATA_DIR: the repositories and their persistence roots are
// intentionally independent after the split.
func RunqdSocketPath() string {
	if socket := os.Getenv("RUNQD_SOCKET"); socket != "" {
		return absoluteClean(socket)
	}
	dataDir := os.Getenv("RUNQD_DATA_DIR")
	if dataDir == "" {
		if os.Geteuid() == 0 {
			dataDir = "/var/lib/runqd"
		} else {
			home, _ := os.UserHomeDir()
			dataDir = filepath.Join(home, ".local", "share", "runqd")
		}
	}
	return filepath.Join(absoluteClean(dataDir), "runqd.sock")
}

func absoluteClean(value string) string {
	abs, err := filepath.Abs(value)
	if err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(value)
}
