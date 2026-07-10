package cli

import (
	"context"
	"encoding/json"
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

// ── runq doctor（静态 × 本机，零主动探测）──
//
// doctor 检查的对象是"这台机器"，不是某个进程。它从不主动探测网络——
// 但 Targets 节会显示每个 target 的状态：local runqd 走本机 socket
// dial（本地 IPC），remote 读 daemon 的 /health 被动缓存（lane 干活时
// 顺手记下的 lastContact，D6），并标注缓存年龄保持诚实。真的主动探测
// 是 `runq config check --live` 的显式动作。按机器角色分节：
//
//	client 节    永远显示——config、data dir/DB/logs、daemon socket、
//	             targets（静态校验 + 状态）、ssh key 文件存在性
//	executor 节  本机有 executor 痕迹时显示——GPU（仅当 local target
//	             声明 gpus 才算失败）、runqd binary/socket
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

	fmt.Println("== runq client ==")
	cfg, health := doctorClient(d)

	doctorConnection(cfg, health)

	if hasExecutorRole(cfg) {
		fmt.Println()
		fmt.Println("== executor ==")
		doctorExecutor(d, cfg)
	}

	d.summary()
	return nil
}

// doctorClient checks the client role: static config + local resources.
// It never PROBES the network — target status comes from the daemon's
// /health PASSIVE cache over the local socket (lanes record reachability
// as a side effect of work they were doing anyway, D6).
func doctorClient(d *doctorChecks) (*config.GlobalConfig, map[string]targetHealthLine) {
	fmt.Println("Config:")
	cfg, err := config.Load()
	if err != nil {
		d.check(false, "", fmt.Sprintf("%s: %v", config.ConfigPath(), err))
	} else {
		d.check(true, config.ConfigPath(), "")
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

	fmt.Println("Logs:")
	if info, err := os.Stat(paths.LogDir); err != nil {
		d.skip(fmt.Sprintf("%s does not exist yet", paths.LogDir))
	} else {
		d.check(true, fmt.Sprintf("%s (%s)", paths.LogDir, info.Mode()), "")
	}

	// Socket dial is local IPC, not network — allowed in doctor.
	fmt.Println("Daemon:")
	daemonUp := checkSocketAlive(paths.SocketPath)
	if daemonUp {
		d.check(true, "running and answering on the socket", "")
	} else {
		d.check(false, "", api.DiagnoseDaemon(paths.SocketPath, paths.PIDPath))
	}

	health := fetchTargetHealth(daemonUp)
	doctorTargets(d, cfg, dataDir, daemonUp, health)
	return cfg, health
}

// doctorTargets renders one line per target: static template validation
// PLUS live status — local runqd via socket dial (local IPC), remote via
// the daemon's passive /health cache (never an active probe; the age of
// the cached answer is shown so staleness is honest).
func doctorTargets(d *doctorChecks, cfg *config.GlobalConfig, dataDir string, daemonUp bool, health map[string]targetHealthLine) {
	fmt.Println("Targets:")
	if cfg == nil || len(cfg.ResolveTargets()) == 0 {
		d.skip("no targets configured — add one: `runq target add <name> --template=<scheduler>`")
		return
	}

	defaultTarget := cfg.ResolveDefaultTarget()

	for _, t := range cfg.ResolveTargets() {
		issues := 0
		for _, r := range t.CheckHPC() {
			if r.Status == "fail" {
				issues++
			}
		}

		label := t.Name
		if t.Name == defaultTarget {
			label = "[default] " + label
		}
		if t.Type() == config.TargetTypeHPC {
			kind := "remote"
			if t.Scheduler != "" {
				kind += "/" + t.Scheduler
			}
			label += " (" + kind + ")"
		} else {
			label += " (local/runqd)"
		}

		var status string
		var ok bool
		if t.Type() != config.TargetTypeHPC {
			// Local runqd: socket dial is local IPC.
			if checkSocketAlive(utils.RunqdPathsFromDataDir(dataDir).SocketPath) {
				status, ok = "connected", true
			} else {
				status, ok = "disconnected (auto-starts on demand)", true // informational, not a failure
			}
		} else if !daemonUp {
			status, ok = "status unknown (daemon offline)", true
		} else if h, found := health[t.Name]; found && h.checked > 0 {
			if h.reachable {
				status, ok = "alive"+h.age, true
			} else {
				status, ok = "unreachable"+h.age+h.lastErr, false
			}
		} else {
			status, ok = "no contact yet (reachability is recorded passively as the daemon works)", true
		}

		switch {
		case issues > 0:
			d.check(false, "", fmt.Sprintf("%s: %d template issue(s) — details: `runq config check %s`", label, issues, t.Name))
		case !ok:
			d.check(false, "", fmt.Sprintf("%s: %s", label, status))
		default:
			d.check(true, fmt.Sprintf("%s: %s", label, status), "")
		}

		// SSH key existence: pure file stat.
		if t.SSH != nil && t.SSH.Key != "" {
			key := t.SSH.Key
			if strings.HasPrefix(key, "~/") {
				if home, herr := os.UserHomeDir(); herr == nil {
					key = filepath.Join(home, key[2:])
				}
			}
			if _, err := os.Stat(key); err != nil {
				d.check(false, "", fmt.Sprintf("%s: ssh key %s: %v", t.Name, t.SSH.Key, err))
			}
		}
	}
}

type targetHealthLine struct {
	reachable bool
	checked   int64  // unix; 0 = no contact since daemon boot
	age       string // " (checked 3m ago)" or ""
	lastErr   string // ": <err>" or ""
}

// fetchTargetHealth reads the daemon's PASSIVE reachability cache via the
// local socket (GET /health, D6). Any failure degrades to "no data".
func fetchTargetHealth(daemonUp bool) map[string]targetHealthLine {
	out := map[string]targetHealthLine{}
	if !daemonUp {
		return out
	}
	client := api.NewClient(getSocketPath())
	resp, err := client.Do(context.Background(), "GET", "/api/v1/health", nil)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	var body struct {
		Targets []struct {
			Name        string `json:"name"`
			Reachable   bool   `json:"reachable"`
			LastError   string `json:"last_error"`
			LastChecked int64  `json:"last_checked"`
		} `json:"targets"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return out
	}
	for _, t := range body.Targets {
		line := targetHealthLine{reachable: t.Reachable, checked: t.LastChecked}
		if t.LastChecked > 0 {
			line.age = fmt.Sprintf(" (checked %s ago)", time.Since(time.Unix(t.LastChecked, 0)).Round(time.Second))
		}
		if t.LastError != "" {
			line.lastErr = ": " + t.LastError
		}
		out[t.Name] = line
	}
	return out
}

// hasExecutorRole reports whether THIS machine is supposed to execute:
// a local target is configured, or the runqd binary is installed. A live
// runqd socket alone is deliberately NOT a role claim — a stale daemon
// from past experiments must not conjure a section full of red.
func hasExecutorRole(cfg *config.GlobalConfig) bool {
	if cfg != nil {
		for _, t := range cfg.ResolveTargets() {
			if t.Type() != config.TargetTypeHPC {
				return true
			}
		}
	}
	return app.FindRunqd() != ""
}

// doctorExecutor checks the executor role: can this machine RUN tasks.
func doctorExecutor(d *doctorChecks, cfg *config.GlobalConfig) {
	// GPU visibility only FAILS when a local target actually declares
	// GPUs — a Mac client with a CPU-only local target should not bleed
	// red over nvidia-smi.
	gpusConfigured := false
	if cfg != nil {
		for _, t := range cfg.ResolveTargets() {
			if t.Type() != config.TargetTypeHPC && len(t.GPUs) > 0 {
				gpusConfigured = true
				break
			}
		}
	}
	fmt.Println("GPU:")
	gpuCount, gpuErr := checkNvidiaSmi()
	switch {
	case gpuErr == nil:
		d.check(true, fmt.Sprintf("nvidia-smi found (%d GPUs detected)", gpuCount), "")
	case gpusConfigured:
		d.check(false, "", fmt.Sprintf("nvidia-smi: %v (local target declares gpus)", gpuErr))
	default:
		d.skip(fmt.Sprintf("nvidia-smi: %v — no local target declares gpus", gpuErr))
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

// ── connection 节：remote target 的 ssh 解析展示 ──
//
// 这节回答的是"连出去用的到底是什么"：target 的 ssh.host 往往是
// ~/.ssh/config 里的 alias（HostName/User/IdentityFile 都藏在那边），
// config.yaml 里 user: "" 看着像配置错误其实不是。静态读文件展示
// 解析结果，可达性照旧引用 /health 被动缓存——这节不做任何计数，
// 纯信息（Targets 节已经为可达性计过 pass/fail 了）。
func doctorConnection(cfg *config.GlobalConfig, health map[string]targetHealthLine) {
	if cfg == nil {
		return
	}
	var remotes []config.TargetConfig
	for _, t := range cfg.ResolveTargets() {
		if t.Type() == config.TargetTypeHPC && t.SSH != nil {
			remotes = append(remotes, t)
		}
	}
	if len(remotes) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("== connection ==")
	for _, t := range remotes {
		avail := "no contact yet"
		if h, found := health[t.Name]; found && h.checked > 0 {
			if h.reachable {
				avail = "available" + h.age
			} else {
				avail = "unreachable" + h.age + h.lastErr
			}
		}
		fmt.Printf("%s: %s\n", t.Name, avail)

		block := sshConfigBlock(t.SSH.Host)
		if len(block) > 0 {
			fmt.Printf("  ~/.ssh/config:\n")
			for _, l := range block {
				fmt.Printf("    %s\n", l)
			}
			if t.SSH.User == "" && !blockHasKeyword(block, "user") {
				fmt.Printf("    # user unset — current OS user (%s) will be used\n", os.Getenv("USER"))
			}
		} else {
			fmt.Printf("  no ~/.ssh/config entry for %q — used as hostname directly\n", t.SSH.Host)
		}
	}
}

// sshConfigBlock finds the Host block in ~/.ssh/config whose pattern
// matches alias and returns its lines (Host line included, comments
// stripped). Pure file read — never touches the network. Returns nil
// when the file or a matching block doesn't exist.
func sshConfigBlock(alias string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	var block []string
	in := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		kw := strings.ToLower(fields[0])
		if kw == "host" || kw == "match" {
			if in {
				break // end of our block
			}
			if kw == "host" {
				for _, pat := range fields[1:] {
					if strings.HasPrefix(pat, "!") {
						if ok, _ := filepath.Match(pat[1:], alias); ok {
							break // negated match excludes this block
						}
						continue
					}
					if ok, _ := filepath.Match(pat, alias); ok {
						in = true
						block = append(block, line)
						break
					}
				}
			}
			continue
		}
		if in {
			block = append(block, line)
		}
	}
	return block
}

// blockHasKeyword reports whether an ssh config block contains keyword
// (case-insensitive first token).
func blockHasKeyword(block []string, keyword string) bool {
	for _, l := range block {
		f := strings.Fields(l)
		if len(f) > 0 && strings.EqualFold(f[0], keyword) {
			return true
		}
	}
	return false
}
