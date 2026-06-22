package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/utils"

	"github.com/spf13/cobra"
)

func init() {
	doctorCmd.GroupID = groupDiag
	rootCmd.AddCommand(doctorCmd)
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system health for the configured mode (daemon or hpc)",
	RunE:  runDoctor,
}

// doctorChecks tallies pass/fail and renders the three-state lines —
// "didn't check" must look different from "checked and passed" (same
// philosophy as preflight).
type doctorChecks struct{ passed, failed int }

func (d *doctorChecks) check(ok bool, pass, fail string) {
	if ok {
		fmt.Printf("  %s %s\n", utils.PassFail(true), pass)
		d.passed++
	} else {
		fmt.Printf("  %s %s\n", utils.PassFail(false), fail)
		d.failed++
	}
}

func (d *doctorChecks) skip(reason string) {
	fmt.Printf("  - skipped (%s)\n", reason)
}

func (d *doctorChecks) summary() {
	fmt.Println()
	if d.failed == 0 {
		fmt.Printf("%s All %d checks passed.\n", utils.PassFail(true), d.passed)
	} else {
		fmt.Printf("%d passed, %s %d failed.\n", d.passed, utils.PassFail(false), d.failed)
	}
}

func runDoctor(cmd *cobra.Command, args []string) error {
	cfg, cfgErr := config.Load()
	mode := config.ModeDaemon
	if cfgErr == nil {
		mode = config.ConfigMode(cfg)
	}
	d := &doctorChecks{}
	if mode == config.ModeHPC {
		runDoctorHPC(d)
	} else {
		runDoctorDaemon(d)
	}
	checkModeConsistency(d)
	d.summary()
	return nil
}

// runDoctorHPC checks what HPC mode actually uses: everything lives under
// ~/.runq (config.yaml, runq.db, logs), scheduling is delegated to the
// cluster CLI. GPU and daemon checks are SKIPPED, not failed — a login node
// without nvidia-smi is normal, not broken.
func runDoctorHPC(d *doctorChecks) {
	dir := config.ConfigDir()

	fmt.Println("GPU:")
	d.skip("hpc mode — GPUs live on compute nodes, not here")

	fmt.Println("Config dir:")
	if info, err := os.Stat(dir); err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v — run `runq hpc init`", dir, err))
	} else if !info.IsDir() {
		d.check(false, "", fmt.Sprintf("%s: exists but is not a directory", dir))
	} else {
		d.check(true, fmt.Sprintf("%s (%s)", dir, info.Mode()), "")
	}

	fmt.Println("HPC config:")
	hpcCfg, err := hpcconfig.Load()
	if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v — run `runq hpc init`", config.ConfigPath(), err))
	} else {
		d.check(true, fmt.Sprintf("%s (validate with `runq hpc config check`)", config.ConfigPath()), "")
	}

	fmt.Println("Scheduler CLI:")
	if hpcCfg == nil || strings.TrimSpace(hpcCfg.SubmitTemplate) == "" {
		d.skip("no submit_template configured yet")
	} else {
		bin := strings.Fields(hpcCfg.SubmitTemplate)[0]
		if path, err := exec.LookPath(bin); err != nil {
			d.check(false, "", fmt.Sprintf("%s: not found in PATH (submit_template's first word)", bin))
		} else {
			d.check(true, fmt.Sprintf("%s → %s", bin, path), "")
		}
	}

	fmt.Println("Database:")
	dbPath := config.DBPath()
	if info, err := os.Stat(dbPath); os.IsNotExist(err) {
		d.skip(fmt.Sprintf("%s does not exist yet — created on first submit", dbPath))
	} else if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", dbPath, err))
	} else if f, err := os.OpenFile(dbPath, os.O_RDWR, 0); err != nil {
		d.check(false, "", fmt.Sprintf("%s: not writable: %v", dbPath, err))
	} else {
		f.Close()
		d.check(true, fmt.Sprintf("%s (%d bytes)", dbPath, info.Size()), "")
	}

	fmt.Println("Daemon:")
	d.skip("hpc mode — the cluster scheduler runs the tasks")

	fmt.Println("Logs:")
	logDir := hpcconfig.LogDir()
	if info, err := os.Stat(logDir); os.IsNotExist(err) {
		d.skip(fmt.Sprintf("%s does not exist yet — created on first submit", logDir))
	} else if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", logDir, err))
	} else {
		d.check(true, fmt.Sprintf("%s (%s) — operation log: %s", logDir, info.Mode(), hpcconfig.OpLogPath()), "")
	}
}

// runDoctorDaemon is the original daemon-mode health check.
func runDoctorDaemon(d *doctorChecks) {
	_, dataDir := utils.ResolveDataDir()
	paths := utils.PathsFromDataDir(dataDir)

	fmt.Println("GPU:")
	gpuCount, gpuErr := checkNvidiaSmi()
	d.check(gpuErr == nil, fmt.Sprintf("nvidia-smi found (%d GPUs detected)", gpuCount), fmt.Sprintf("nvidia-smi: %v", gpuErr))

	fmt.Println("Data dir:")
	info, err := os.Stat(paths.DataDir)
	if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", paths.DataDir, err))
	} else if !info.IsDir() {
		d.check(false, "", fmt.Sprintf("%s: exists but is not a directory", paths.DataDir))
	} else {
		d.check(true, fmt.Sprintf("%s (%s)", paths.DataDir, info.Mode()), "")
	}

	fmt.Println("Database:")
	dbInfo, err := os.Stat(paths.DBPath)
	if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", paths.DBPath, err))
	} else {
		f, err := os.OpenFile(paths.DBPath, os.O_RDWR, 0)
		if err != nil {
			d.check(false, "", fmt.Sprintf("%s: not writable: %v", paths.DBPath, err))
		} else {
			f.Close()
			d.check(true, fmt.Sprintf("%s (%s, %d bytes)", paths.DBPath, dbInfo.Mode(), dbInfo.Size()), "")
		}
	}

	fmt.Println("Daemon:")
	if checkDaemonAlive(paths.SocketPath) {
		d.check(true, "daemon is running and responding", "")
	} else {
		d.check(false, "", api.DiagnoseDaemon(paths.SocketPath, paths.PIDPath))
	}

	fmt.Println("Logs:")
	logInfo, err := os.Stat(paths.LogDir)
	if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", paths.LogDir, err))
	} else {
		tmpPath := paths.LogDir + "/.doctor-check"
		if err := os.WriteFile(tmpPath, []byte("ok"), 0o644); err != nil {
			d.check(false, "", fmt.Sprintf("%s: not writable: %v", paths.LogDir, err))
		} else {
			os.Remove(tmpPath)
			d.check(true, fmt.Sprintf("%s (%s)", paths.LogDir, logInfo.Mode()), "")
		}
	}
}

// checkModeConsistency: an hpc: section with mode=daemon almost always means
// a forgotten `runq config set mode=hpc` after `hpc init`.
func checkModeConsistency(d *doctorChecks) {
	fmt.Println("Mode:")
	cfg, err := config.Load()
	if err != nil {
		d.check(false, "", fmt.Sprintf("config.yaml: %v", err))
		return
	}
	mode := config.ConfigMode(cfg)
	_, hpcErr := hpcconfig.Load()
	hpcConfigured := hpcErr == nil
	switch {
	case hpcConfigured && mode != config.ModeHPC:
		d.check(false, "", fmt.Sprintf("hpc: section is configured but mode is %q — run `runq config set mode=hpc` (or use --mode)", mode))
	case !hpcConfigured && mode == config.ModeHPC:
		d.check(false, "", "mode is hpc but the hpc: section is missing — run `runq hpc init`")
	default:
		d.check(true, fmt.Sprintf("mode %s, config consistent", mode), "")
	}
}

// checkNvidiaSmi runs nvidia-smi and returns the number of GPUs detected.
func checkNvidiaSmi() (int, error) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return 0, fmt.Errorf("not found in PATH")
	}
	out, err := exec.Command("nvidia-smi", "--query-gpu=index", "--format=csv,noheader").Output()
	if err != nil {
		return 0, fmt.Errorf("failed to run: %v", err)
	}
	count := 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(l) != "" {
			count++
		}
	}
	return count, nil
}

// checkDaemonAlive tries to connect to the daemon socket.
func checkDaemonAlive(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
