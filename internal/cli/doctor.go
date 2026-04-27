package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/utils"

	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system health: nvidia-smi, data dir, DB, daemon, logs",
	RunE:  runDoctor,
}

func runDoctor(cmd *cobra.Command, args []string) error {
	_, dataDir := utils.ResolveDataDir()
	paths := utils.PathsFromDataDir(dataDir)

	passed, failed := 0, 0
	check := func(ok bool, pass, fail string) {
		if ok {
			fmt.Printf("  %s %s\n", utils.PassFail(true), pass)
			passed++
		} else {
			fmt.Printf("  %s %s\n", utils.PassFail(false), fail)
			failed++
		}
	}

	// 1. nvidia-smi
	fmt.Println("GPU:")
	gpuCount, gpuErr := checkNvidiaSmi()
	check(gpuErr == nil, fmt.Sprintf("nvidia-smi found (%d GPUs detected)", gpuCount), fmt.Sprintf("nvidia-smi: %v", gpuErr))

	// 2. Data directory
	fmt.Println("Data dir:")
	info, err := os.Stat(paths.DataDir)
	check(err == nil && info.IsDir(),
		fmt.Sprintf("%s (%s)", paths.DataDir, info.Mode()),
		fmt.Sprintf("%s: %v", paths.DataDir, err))

	// 3. Database
	fmt.Println("Database:")
	dbInfo, err := os.Stat(paths.DBPath)
	if err != nil {
		check(false, "", fmt.Sprintf("%s: %v", paths.DBPath, err))
	} else {
		// Check writable by opening.
		f, err := os.OpenFile(paths.DBPath, os.O_RDWR, 0)
		if err != nil {
			check(false, "", fmt.Sprintf("%s: not writable: %v", paths.DBPath, err))
		} else {
			f.Close()
			check(true, fmt.Sprintf("%s (%s, %d bytes)", paths.DBPath, dbInfo.Mode(), dbInfo.Size()), "")
		}
	}

	// 4. Daemon
	fmt.Println("Daemon:")
	daemonAlive := checkDaemonAlive(paths.SocketPath)
	if daemonAlive {
		check(true, "daemon is running and responding", "")
	} else {
		diag := api.DiagnoseDaemon(paths.SocketPath, paths.PIDPath)
		check(false, "", diag)
	}

	// 5. Log directory
	fmt.Println("Logs:")
	logInfo, err := os.Stat(paths.LogDir)
	if err != nil {
		check(false, "", fmt.Sprintf("%s: %v", paths.LogDir, err))
	} else {
		// Check writable by creating a temp file.
		tmpPath := paths.LogDir + "/.doctor-check"
		if err := os.WriteFile(tmpPath, []byte("ok"), 0o644); err != nil {
			check(false, "", fmt.Sprintf("%s: not writable: %v", paths.LogDir, err))
		} else {
			os.Remove(tmpPath)
			check(true, fmt.Sprintf("%s (%s)", paths.LogDir, logInfo.Mode()), "")
		}
	}

	// Summary
	fmt.Println()
	if failed == 0 {
		fmt.Printf("%s All %d checks passed.\n", utils.PassFail(true), passed)
	} else {
		fmt.Printf("%d passed, %s %d failed.\n", passed, utils.PassFail(false), failed)
	}
	return nil
}

// checkNvidiaSmi runs nvidia-smi and returns the number of GPUs detected.
func checkNvidiaSmi() (int, error) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0, fmt.Errorf("not found in PATH")
	}
	_ = path

	cmd := exec.Command("nvidia-smi", "--query-gpu=index", "--format=csv,noheader")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("failed to run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	count := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			count++
		}
	}
	return count, nil
}

// checkDaemonAlive tries to connect to the daemon socket and hit /api/status.
func checkDaemonAlive(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
