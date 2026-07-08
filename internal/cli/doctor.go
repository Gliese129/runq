package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/app"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/utils"

	"github.com/spf13/cobra"
)

// ── runq doctor（无 daemon，静态 × 本机）──
//
// doctor 检查的对象是"这台机器"，不是某个进程：静态配置 + 本机资源，
// 不碰网络（可达性是 /health 的运行时领域；主动探测是 `runq config
// check --live` 的显式动作）。按机器扮演的角色分节：
//
//	client 节    永远显示——config.yaml、targets 静态校验、data dir/DB、
//	             client daemon socket、ssh key 文件存在性
//	executor 节  本机有 executor 痕迹时显示（runqd binary / local target /
//	             runqd socket）——GPU 可见性、runqd socket、执行环境
//
// 同一命令，视角跟着机器走：笔记本上查 client（+本地 executor），
// cp 到 HPC 的二进制在登录节点上查的就是登录节点。mode 二分已死。

func init() {
	doctorCmd.GroupID = groupDiag
	rootCmd.AddCommand(doctorCmd)
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Static self-check of THIS machine (client + executor roles)",
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
	d := &doctorChecks{}

	fmt.Println("== client ==")
	cfg := doctorClient(d)

	if hasExecutorRole(cfg) {
		fmt.Println()
		fmt.Println("== executor ==")
		doctorExecutor(d)
	}

	d.summary()
	return nil
}

// doctorClient checks the client role: static config + local resources.
// Never touches the network — a broken laptop must be diagnosable offline.
func doctorClient(d *doctorChecks) *config.GlobalConfig {
	fmt.Println("Config:")
	cfg, err := config.Load()
	if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", config.ConfigPath(), err))
	} else {
		d.check(true, config.ConfigPath(), "")
	}

	// Targets: static template validation per target (same rules as
	// `runq config check <name>`), plus ssh key file existence — file
	// checks only, no dialing.
	fmt.Println("Targets:")
	if cfg == nil || len(cfg.ResolveTargets()) == 0 {
		d.skip("no targets configured — add one: `runq config add <name> --template=<scheduler>`")
	} else {
		for _, t := range cfg.ResolveTargets() {
			issues := 0
			for _, r := range t.CheckHPC() {
				if r.Status == "fail" {
					issues++
				}
			}
			label := fmt.Sprintf("%s (%s", t.Name, t.Type())
			if t.Scheduler != "" {
				label += "/" + t.Scheduler
			}
			label += ")"
			if issues > 0 {
				d.check(false, "", fmt.Sprintf("%s: %d template issue(s) — details: `runq config check %s`", label, issues, t.Name))
			} else {
				d.check(true, label, "")
			}
			if t.SSH != nil && t.SSH.Key != "" {
				key := t.SSH.Key
				if strings.HasPrefix(key, "~/") {
					if home, herr := os.UserHomeDir(); herr == nil {
						key = filepath.Join(home, key[2:])
					}
				}
				if _, err := os.Stat(key); err != nil {
					d.check(false, "", fmt.Sprintf("%s: ssh key %s: %v", t.Name, t.SSH.Key, err))
				} else {
					d.check(true, fmt.Sprintf("%s: ssh key %s", t.Name, t.SSH.Key), "")
				}
			}
		}
	}

	_, dataDir := utils.ResolveDataDir()
	paths := utils.PathsFromDataDir(dataDir)

	fmt.Println("Data dir:")
	if info, err := os.Stat(paths.DataDir); err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", paths.DataDir, err))
	} else if !info.IsDir() {
		d.check(false, "", fmt.Sprintf("%s: exists but is not a directory", paths.DataDir))
	} else {
		d.check(true, fmt.Sprintf("%s (%s)", paths.DataDir, info.Mode()), "")
	}

	fmt.Println("Database:")
	if dbInfo, err := os.Stat(paths.DBPath); os.IsNotExist(err) {
		d.skip(fmt.Sprintf("%s does not exist yet — created on first submit", paths.DBPath))
	} else if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", paths.DBPath, err))
	} else if f, ferr := os.OpenFile(paths.DBPath, os.O_RDWR, 0); ferr != nil {
		d.check(false, "", fmt.Sprintf("%s: not writable: %v", paths.DBPath, ferr))
	} else {
		f.Close()
		d.check(true, fmt.Sprintf("%s (%d bytes)", paths.DBPath, dbInfo.Size()), "")
	}

	// Socket dial is local IPC, not network — allowed in doctor.
	fmt.Println("Client daemon:")
	if checkSocketAlive(paths.SocketPath) {
		d.check(true, "running and answering on the socket", "")
	} else {
		d.check(false, "", api.DiagnoseDaemon(paths.SocketPath, paths.PIDPath))
	}

	return cfg
}

// hasExecutorRole reports whether THIS machine shows executor traces:
// a local-GPUs target, a runqd sibling binary, or a live runqd socket.
func hasExecutorRole(cfg *config.GlobalConfig) bool {
	if cfg != nil {
		for _, t := range cfg.ResolveTargets() {
			if t.Type() != config.TargetTypeHPC {
				return true
			}
		}
	}
	if app.FindRunqd() != "" {
		return true
	}
	_, dataDir := utils.ResolveDataDir()
	return checkSocketAlive(utils.RunqdPathsFromDataDir(dataDir).SocketPath)
}

// doctorExecutor checks the executor role: can this machine RUN tasks.
func doctorExecutor(d *doctorChecks) {
	fmt.Println("GPU:")
	gpuCount, gpuErr := checkNvidiaSmi()
	if gpuErr != nil {
		d.check(false, "", fmt.Sprintf("nvidia-smi: %v", gpuErr))
	} else {
		d.check(true, fmt.Sprintf("nvidia-smi found (%d GPUs detected)", gpuCount), "")
	}

	fmt.Println("runqd binary:")
	if bin := app.FindRunqd(); bin != "" {
		d.check(true, bin, "")
	} else {
		d.check(false, "", "not found next to runq or on PATH — local targets cannot execute")
	}

	fmt.Println("runqd daemon:")
	_, dataDir := utils.ResolveDataDir()
	socket := utils.RunqdPathsFromDataDir(dataDir).SocketPath
	if checkSocketAlive(socket) {
		d.check(true, "running and answering on the socket", "")
	} else {
		d.skip("not running — the client daemon auto-starts it on demand (ensure-running)")
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

// checkSocketAlive tries to connect to a unix socket (local IPC).
func checkSocketAlive(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
