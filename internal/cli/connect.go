package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gliese129/runq-lab/internal/api"
	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/utils"
	"github.com/gliese129/runq-lab/internal/version"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// ── runq connect：连接一个已配置的 remote target（RQ-45）──
//
// "connect" 是动词本义：跟 target 的 login node 建立可用的连接——试连、
// 装载远端 CLI、打开 socket 转发。它不负责添加 target：配置归 `runq
// target add`，连接归这里，target 不存在直接报错指向前者。
//
// 幂等：重跑 `runq connect <name>` 就是"刷新远端"——升级本地 runq 后用它
// 把新二进制推到 login node。
//
// 安全上它是 host key 信任的"仪式现场"：daemon 的后台连接一律严格校验
// known_hosts，而首次信任必须发生在有人看着指纹的地方——就是这里
// （TOFU 指纹确认，同意后写 known_hosts）。

func init() {
	// connectCmd itself lives under `runq target` (target_cmd.go). The
	// top-level `runq connect` is a thin alias — the first command a new
	// user runs deserves a short spelling.
	rootCmd.AddCommand(connectAliasCmd)
}

var connectAliasCmd = &cobra.Command{
	Use:          "connect [name...]",
	Short:        "Alias of `runq target connect`",
	Args:         cobra.ArbitraryArgs,
	RunE:         runConnect,
	GroupID:      groupDiag,
	SilenceUsage: true,
}

var connectCmd = &cobra.Command{
	Use:   "connect [name...]",
	Short: "Connect configured targets: verify SSH, install the remote CLI, enable the socket forward",
	Args:  cobra.ArbitraryArgs,
	RunE:  runConnect,
	// Business errors ("connection failed") are not usage errors — don't
	// bury them under the flags listing.
	SilenceUsage: true,
}

func runConnect(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	names := args
	if len(names) == 0 {
		if n := promptLine("Target name: "); n != "" {
			names = []string{n}
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("a target name is required")
	}

	// Multiple targets connect sequentially (TOFU prompts must not
	// interleave); one failure doesn't strand the rest.
	var failed []string
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(names) > 1 {
			fmt.Printf("\n── %s ──\n", name)
		}
		if err := connectOne(cmd, cfg, name); err != nil {
			fmt.Printf("✗ %s: %v\n", name, err)
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed to connect: %s", strings.Join(failed, ", "))
	}
	return nil
}

func connectOne(cmd *cobra.Command, cfg *config.GlobalConfig, name string) error {
	var tc *config.TargetConfig
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == name {
			tc = &cfg.Targets[i]
			break
		}
	}
	if tc == nil {
		return fmt.Errorf("target %q not found — add it first: `runq target add %s --template=<scheduler> --host=<login-node>`", name, name)
	}
	if tc.SSH == nil {
		return fmt.Errorf("target %q has no ssh section (local target) — nothing to connect to", name)
	}

	// ── connection test with the stored SSH config; TOFU prompt only when
	// the host key is genuinely unknown ──
	// `host:` may be an ~/.ssh/config alias — resolve it the way OpenSSH
	// would (explicit target fields win) and SHOW the expansion, so the
	// user sees the same thing `ssh -G` would tell them.
	sshHost, sshPort, sshUser, sshKey := rfs.ResolveSSHConfigDefaults(
		tc.SSH.Host, tc.SSH.Port, tc.SSH.User, tc.SSH.Key)
	host := sshHost
	if sshPort > 0 {
		host = fmt.Sprintf("%s:%d", sshHost, sshPort)
	}
	if sshHost != tc.SSH.Host {
		fmt.Printf("Connecting to %s (%s via ~/.ssh/config) ...\n", host, tc.SSH.Host)
	} else {
		fmt.Printf("Connecting to %s ...\n", host)
	}
	// RQ-74: `StrictHostKeyChecking accept-new` in ~/.ssh/config skips the
	// TOFU prompt — the user already told ssh to trust new hosts silently,
	// and runq follows their config instead of adding ceremony. One line
	// says so (self-report: silence is fine, invisibility is not).
	var hostKeys ssh.HostKeyCallback
	var err error
	if rfs.ResolveHostKeyPolicy(tc.SSH.Host) == rfs.HostKeyAcceptNew {
		fmt.Println("host key: new hosts auto-trusted (StrictHostKeyChecking accept-new in ~/.ssh/config)")
		hostKeys, err = rfs.AcceptNewHostKeyCallback()
	} else {
		hostKeys, err = rfs.TOFUHostKeyCallback(func(h, fingerprint string) bool {
			fmt.Printf("\nThe authenticity of host %q can't be established.\n", h)
			fmt.Printf("Key fingerprint: %s\n", fingerprint)
			fmt.Println("Verify it against the cluster's published fingerprint if you can.")
			return confirmYN("Trust this host and add it to ~/.ssh/known_hosts?")
		})
	}
	if err != nil {
		return err
	}
	auth, err := rfs.ResolveAuthMethods(sshKey)
	if err != nil {
		return err
	}
	fsys := rfs.NewSSHFS(rfs.SSHConfig{
		Host:            host,
		User:            sshUser,
		AuthMethods:     auth,
		HostKeyCallback: hostKeys,
		IdleTimeout:     time.Minute,
	})
	defer fsys.Close()

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	if _, _, _, err := fsys.Exec(ctx, "true"); err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	fmt.Println("✓ connected")

	// ── provision the remote CLI and enable the forward ──
	setupRemoteCLI(cmd.Context(), fsys, name)
	if !tc.RemoteCLI {
		tc.RemoteCLI = true
		if err := config.SaveGlobal(cfg); err != nil {
			return err
		}
		fmt.Printf("✓ remote_cli enabled for %q\n", name)
	}
	notifyDaemonForward(cmd.Context(), name)
	return nil
}

// notifyDaemonForward asks the RUNNING daemon to start the forward against
// the config we just saved — the happy path needs no restart at all. Every
// other outcome degrades to an honest instruction: daemon not running →
// the forward comes up with it; lane missing (target added after daemon
// start) → restart is genuinely required; old daemon without the endpoint
// → restart covers it too.
func notifyDaemonForward(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := api.NewClient(getSocketPath())
	resp, err := client.Do(ctx, http.MethodPost, "/api/v1/targets/"+url.PathEscape(name)+"/connect", nil)
	if err != nil {
		fmt.Println("Daemon not running — the socket forward will come up when it starts (`runq daemon start`).")
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		fmt.Println("✓ socket forward starting (daemon notified — no restart needed)")
	case http.StatusConflict, http.StatusNotImplemented, http.StatusNotFound:
		// 409: lane missing. 501/404: older daemon build without the
		// endpoint. Either way a restart is what actually helps.
		fmt.Println("Next: restart the daemon (`runq daemon restart`) to pick up this target and its socket forward.")
	default:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		fmt.Printf("! daemon could not start the forward: %s\n", e.Error)
	}
}

// setupRemoteCLI provisions the login node for remote CLI use: a matching
// runq binary in ~/.runq/bin and an env file wiring PATH, the forwarded
// socket, and the default target. The socket itself appears when the
// daemon (re)starts and establishes the forward.
func setupRemoteCLI(ctx context.Context, fsys *rfs.SSHFS, targetName string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	home, err := fsys.Home()
	if err != nil {
		fmt.Printf("! remote CLI setup skipped (cannot resolve remote home): %v\n", err)
		return
	}
	runqDir := home + "/.runq"
	binPath := runqDir + "/bin/runq"
	// 0700 on ~/.runq is the access boundary: the forwarded socket inside
	// it controls this machine's daemon, and login nodes are multi-user.
	if err := fsys.MkdirAll(runqDir+"/bin", 0o700); err != nil {
		fmt.Printf("! remote CLI setup failed (mkdir): %v\n", err)
		return
	}

	// Install the binary only when the remote one is missing or a
	// different build. `runq version` prints exactly the version — that
	// output is the contract this comparison rests on.
	stdout, _, _, err := fsys.Exec(ctx, "sh", "-c",
		fmt.Sprintf("%s version 2>/dev/null; true", utils.ShellQuote(binPath)))
	remoteVersion := strings.TrimSpace(string(stdout))
	switch {
	case err == nil && remoteVersion != "" && remoteVersion == version.Version && remoteVersion != "dev":
		fmt.Printf("✓ remote runq %s already installed\n", remoteVersion)
	default:
		if installRemoteBinary(ctx, fsys, binPath) {
			fmt.Printf("✓ runq installed to %s\n", binPath)
		}
	}

	// Env file: sourced from the user's shell rc. RUNQ_SOCKET points the
	// remote CLI at the forwarded socket; RUNQ_TARGET scopes it to this
	// target so path semantics line up (paths resolve on this very host).
	env := fmt.Sprintf(`# Managed by 'runq connect' — add to your shell rc:
#   echo 'source ~/.runq/env' >> ~/.bashrc
export PATH="$HOME/.runq/bin:$PATH"
export RUNQ_SOCKET="$HOME/.runq/runq.sock"
export RUNQ_TARGET=%s
`, utils.ShellQuote(targetName))
	if err := fsys.WriteFile(runqDir+"/env", []byte(env), 0o644); err != nil {
		fmt.Printf("! writing %s/env failed: %v\n", runqDir, err)
		return
	}
	fmt.Printf("✓ remote env written — on the cluster, run: echo 'source ~/.runq/env' >> ~/.bashrc\n")
}

// installRemoteBinary uploads THIS executable when the remote platform
// matches, atomically (tmp + rename — overwriting a running binary in
// place would hit ETXTBSY). Cross-platform installs (mac workstation →
// linux cluster) can't ship self; print instructions instead of guessing.
func installRemoteBinary(ctx context.Context, fsys *rfs.SSHFS, binPath string) bool {
	stdout, _, _, err := fsys.Exec(ctx, "uname", "-sm")
	if err != nil {
		fmt.Printf("! remote platform probe failed: %v\n", err)
		return false
	}
	if !unameMatchesSelf(string(stdout)) {
		fmt.Printf("! this machine is %s/%s but the remote is %q — install a matching runq build to %s manually (or via your lab's module system)\n",
			runtime.GOOS, runtime.GOARCH, strings.TrimSpace(string(stdout)), binPath)
		return false
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Printf("! cannot locate the local runq binary: %v\n", err)
		return false
	}
	data, err := os.ReadFile(self)
	if err != nil {
		fmt.Printf("! reading local runq binary failed: %v\n", err)
		return false
	}
	tmp := binPath + ".tmp"
	if err := fsys.WriteFile(tmp, data, 0o755); err != nil {
		fmt.Printf("! uploading runq failed: %v\n", err)
		return false
	}
	if err := fsys.Rename(tmp, binPath); err != nil {
		fmt.Printf("! installing runq failed: %v\n", err)
		return false
	}
	return true
}

// unameMatchesSelf reports whether `uname -sm` output describes the same
// platform as this build — the precondition for shipping our own binary.
func unameMatchesSelf(unameSM string) bool {
	fields := strings.Fields(unameSM)
	if len(fields) != 2 {
		return false
	}
	if !strings.EqualFold(fields[0], runtime.GOOS) {
		return false
	}
	arch := strings.ToLower(fields[1])
	switch runtime.GOARCH {
	case "amd64":
		return arch == "x86_64" || arch == "amd64"
	case "arm64":
		return arch == "aarch64" || arch == "arm64"
	default:
		return arch == runtime.GOARCH
	}
}

// promptLine reads one trimmed line from stdin.
func promptLine(prompt string) string {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}
