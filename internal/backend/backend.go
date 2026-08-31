// Package backend defines the Backend interface and its implementations.
//
// File layout:
//
//	interfaces.go          — Backend interface + SubmitOptions + Capabilities
//	errors.go              — ErrNotSupported, ErrNotFound, IsNotFound
//	helpers.go             — Build* view builders, metric readers, BuildDryRunResult
//	types.go               — view/response structs
//	clean.go               — PerformClean shared logic
//	reconciler.go          — Reconciler interface, NoopReconciler, DefaultReadTTL
//
//	store_queries.go       — storeQueries base struct (store + registry delegation)
//	ssh_backend.go         — SSHBackend: HPC/runqd execution lanes
//	multi.go               — MultiBackend: routes by target name
//	unavailable_backend.go — test/fallback stub
//
// The CLI-side proxy (formerly DaemonBackend) lives in package api as api.Proxy.
package backend
