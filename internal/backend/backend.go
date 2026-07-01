// Package backend defines the Backend interface and its implementations.
//
// File layout:
//
//	interfaces.go          — Backend interface + SubmitOptions + Capabilities
//	errors.go              — ErrNotSupported, ErrNotFound, IsNotFound
//	helpers.go             — Build* view builders, metric readers, buildDryRunResult
//	types.go               — view/response structs
//	clean.go               — PerformClean shared logic
//	reconciler.go          — Reconciler interface, NoopReconciler, DefaultReadTTL
//
//	store_backend.go       — storeBackend base struct (store + registry delegation)
//	local_backend.go       — LocalBackend: in-process daemon services (local GPU)
//	ssh_backend.go         — SSHBackend: remote HPC cluster via SSH
//	multi.go               — MultiBackend: routes by target name
//	daemon_backend.go      — DaemonBackend: CLI HTTP proxy to daemon socket
//	unavailable_backend.go — test/fallback stub
package backend
