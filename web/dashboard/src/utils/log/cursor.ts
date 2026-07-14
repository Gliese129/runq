// Pure page-application state machine for the log stream contract v2.
//
// Every page (SSE event, GET catch-up, Load more) flows through applyPage,
// which classifies it against the client's byte cursor into ONE action:
//
//   reset   — page.rotated: the file shrank below our cursor; the server
//             restarted from offset 0. Clear the buffer and start over.
//   append  — page.offset === our endOffset: the page continues the
//             buffer. `mergeFirst` says the first entry is a fragment to
//             be glued onto the buffer's last line (continues chain).
//   ignore  — duplicate/stale page fully behind the cursor (GET × SSE
//             races deliver the same bytes twice), or the empty
//             steady-state page. No state changes.
//   resync  — cursor invariant violated (gap ahead, partial overlap):
//             the caller must NOT apply the page; it re-fetches from its
//             endOffset (one GET), and reloads wholesale if that fails.
//
// Kept free of Vue reactivity so the four failure chains (half-line race,
// rotation, out-of-order, duplicate page) are exhaustively unit-testable.
import type { LogPage } from '@/types/api'

export interface CursorState {
  /** Byte offset the buffer ends at — the next expected page.offset. */
  endOffset: number
  /** The buffer's last line is an unterminated fragment (page.partial). */
  tailPartial: boolean
}

export type CursorAction =
  | {
      kind: 'reset'
      lines: string[]
      nextOffset: number
      size: number
      tailPartial: boolean
    }
  | {
      kind: 'append'
      lines: string[]
      /** Glue lines[0] onto the buffer's last line (fragment chain). */
      mergeFirst: boolean
      nextOffset: number
      size: number
      tailPartial: boolean
    }
  | { kind: 'ignore' }
  | { kind: 'resync' }

export function applyPage(state: CursorState, page: LogPage): CursorAction {
  const lines = page.lines ?? []

  if (page.rotated) {
    return {
      kind: 'reset',
      lines,
      nextOffset: page.next_offset,
      size: page.size,
      tailPartial: !!page.partial,
    }
  }

  if (page.offset === state.endOffset) {
    // Steady-state empty page (caught up / EOF short-line wait): no-op.
    if (lines.length === 0 && page.next_offset === state.endOffset) {
      return { kind: 'ignore' }
    }
    return {
      kind: 'append',
      lines,
      // Trust the server's continues flag only when we also believe our
      // tail is unterminated. continues against a complete tail happens
      // legitimately once: opening at the tail INSIDE a mega-line (empty
      // buffer) — the caller pushes the fragment as a fresh line then.
      mergeFirst: !!page.continues && state.tailPartial,
      nextOffset: page.next_offset,
      size: page.size,
      tailPartial: !!page.partial,
    }
  }

  // Fully behind the cursor: a duplicate delivery (GET × SSE race) whose
  // bytes are already in the buffer. Drop it without touching state.
  if (page.offset < state.endOffset && page.next_offset <= state.endOffset) {
    return { kind: 'ignore' }
  }

  // Anything else — a gap ahead of the cursor (missed page) or a partial
  // overlap (page straddles our endOffset; the half-line race lands here
  // when a raced GET already consumed the continuation). Applying it
  // would corrupt the buffer; the caller resyncs instead.
  return { kind: 'resync' }
}
