// Selection semantics for glob params (RQ2-3, Codex r1 F1).
//
// The submitted selection (row.values) survives three kinds of resolution:
//
//   fresh row        — nothing selected, no baseline → select everything
//   hydrated row     — a snapshot (?fromJob / draft) selected a subset
//                      BEFORE any scan ran → the scan must keep the frozen
//                      subset (intersection), never expand it: config_json
//                      snapshots are the reproducibility story
//   rescan           — a baseline exists → keep what's still selected and
//                      ADOPT new arrivals (a pattern means "these files";
//                      a checkpoint written since the last scan belongs to
//                      the sweep unless the user says otherwise)
//
// missing reports selected paths the resolution no longer matches — they
// are dropped from the selection but must be surfaced, never silent.

export interface GlobSelectionResult {
  next: string[]
  missing: string[]
}

export function mergeGlobSelection(
  prevCandidates: string[],
  selected: string[],
  resolved: string[],
): GlobSelectionResult {
  const sel = new Set(selected)
  const missing = selected.filter(p => !resolved.includes(p))
  if (prevCandidates.length === 0) {
    // First successful scan: fresh row → all; hydrated snapshot → freeze.
    const next = sel.size === 0 ? [...resolved] : resolved.filter(p => sel.has(p))
    return { next, missing }
  }
  const prev = new Set(prevCandidates)
  return { next: resolved.filter(p => sel.has(p) || !prev.has(p)), missing }
}
