// shortcuts.go previously contained all top-level CLI commands. They have been
// split into per-command files: submit_cmd.go, run_cmd.go, ps_cmd.go,
// logs_cmd.go, kill_cmd.go, gpu_cmd.go, status_cmd.go. Each file owns its
// command variable, flags, and init() registration.
package cli
