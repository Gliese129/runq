// Ring-buffer trim for follow-mode log buffers (RQ-51 C-layer).
//
// Follow mode appends forever; this caps JS memory. 20k lines covers most
// complete training logs (so most users never trim), and with the
// incremental pipeline the ONLY cost of the bigger buffer is the
// occasional rebuild after a trim. Hysteresis batches that cost: trim
// fires every `hysteresis` lines, not per push. The backend log file is
// never affected — trimmed lines remain fetchable.
export const LOG_BUFFER_LIMIT = 20_000
export const LOG_BUFFER_HYSTERESIS = 4_000

/**
 * Trim `lines` IN PLACE down to `limit` once it exceeds limit+hysteresis.
 * Returns how many lines were dropped (0 = untouched). Callers only trim
 * while pinned to the tail (follow) — never while the user reads history.
 */
export function trimLogBuffer(
  lines: string[],
  limit = LOG_BUFFER_LIMIT,
  hysteresis = LOG_BUFFER_HYSTERESIS,
): number {
  if (lines.length <= limit + hysteresis) return 0
  const drop = lines.length - limit
  lines.splice(0, drop)
  return drop
}
