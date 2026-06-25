// Package backend defines the Backend interface and shared view builders.
//
// File layout:
//   - interfaces.go — Backend interface + SubmitOptions
//   - errors.go     — ErrNotSupported, ErrNotFound, IsNotFound
//   - helpers.go    — Build* view builders, metric readers, buildDryRunResult
//   - types.go      — view/response structs
//   - clean.go      — PerformClean shared logic
//
// Implementations live in hpc_backend.go, daemon_backend.go, unavailable_backend.go.
package backend
