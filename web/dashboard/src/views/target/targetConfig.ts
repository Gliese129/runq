// targetConfig (RQ2-4 c4, Codex F1) — stable canonical form for dirty
// comparison of a TargetConfig. The first version used
// JSON.stringify(cfg, Object.keys(cfg).sort()) — but an ARRAY replacer
// is a key whitelist applied recursively at every level, so nested ssh
// keys (host/user/proxy_jump) were filtered out of both sides and SSH
// edits never read as dirty. This is a real recursive canonicalization:
// keys sorted at every depth, undefined dropped, empty arrays/objects
// that mean "absent" normalized away.
import type { TargetConfig } from '@/apis/config'

function canonical(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(canonical)
  if (v && typeof v === 'object') {
    const out: Record<string, unknown> = {}
    for (const k of Object.keys(v as Record<string, unknown>).sort()) {
      const val = (v as Record<string, unknown>)[k]
      // In this config's vocabulary "" and absent are the same statement
      // (every string field means "not set" when empty; the Go side even
      // serializes some nested empties, e.g. ssh.user has no omitempty) —
      // treating them as different made a clean form read as dirty.
      if (val === undefined || val === '') continue
      out[k] = canonical(val)
    }
    return out
  }
  return v
}

/** Canonical string of any config VALUE (e.g. the ssh sub-object) —
 *  same ""-equals-absent rule, for per-field conflict comparison. */
export function canonicalValue(v: unknown): string {
  return JSON.stringify(canonical(v) ?? null)
}

/** Canonical string of a target config — equal strings ⇔ same config.
 *  An empty status_parser and an absent one are the same statement. */
export function canonicalTarget(cfg: TargetConfig | null): string {
  if (!cfg) return ''
  const c: Record<string, unknown> = { ...cfg }
  if (Array.isArray(c.status_parser) && c.status_parser.length === 0) delete c.status_parser
  return JSON.stringify(canonical(c))
}
