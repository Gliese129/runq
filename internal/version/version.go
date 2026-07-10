// Package version holds the build version stamped at release time.
//
// Version is the ONE version number for the whole system — the CLI reports
// it in X-Runq-Version, the daemon reports it in /api/v1/health and its
// response headers, and `runq connect` compares local vs remote `runq
// version` output to decide whether the remote binary needs reinstalling.
// One number, three uses; do not invent per-surface versions.
package version

// Version is the build version, stamped via:
//
//	go build -ldflags "-X github.com/gliese129/runq/internal/version.Version=v0.4.0"
//
// "dev" means an unstamped local build. Dev builds are exempt from the
// client gate on both sides: a gate between two workstation builds would
// only punish development.
var Version = "dev"

// MinClient is the oldest stamped client version the daemon accepts on
// /api/v1 (the 426 gate). Empty disables the gate. Bump this only when a
// wire change actually breaks older clients — the gate is a guardrail
// against stale remote installs, not a negotiation protocol; /api/v1 path
// versioning owns protocol-shape compatibility.
var MinClient = ""

// Compare compares two stamped versions ("v1.2.3" or "1.2.3", numeric
// fields, missing fields are zero). ok is false when either side is not a
// stamped version (e.g. "dev"), in which case callers must not gate.
func Compare(a, b string) (cmp int, ok bool) {
	av, aok := parse(a)
	bv, bok := parse(b)
	if !aok || !bok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parse extracts up to three numeric fields from "v1.2.3". A trailing
// pre-release suffix on the last field ("3-rc1") is truncated at the first
// non-digit — precise pre-release ordering is out of scope for a guardrail.
func parse(v string) (out [3]int, ok bool) {
	if len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		v = v[1:]
	}
	if v == "" {
		return out, false
	}
	field, idx := 0, 0
	for idx < len(v) && field < 3 {
		start := idx
		n := 0
		for idx < len(v) && v[idx] >= '0' && v[idx] <= '9' {
			n = n*10 + int(v[idx]-'0')
			idx++
		}
		if idx == start {
			return out, false // field must start with a digit
		}
		out[field] = n
		field++
		// Skip to the next field: stop at '.', tolerate a suffix like "-rc1".
		for idx < len(v) && v[idx] != '.' {
			idx++
		}
		if idx < len(v) {
			idx++ // consume '.'
		}
	}
	return out, field > 0
}
