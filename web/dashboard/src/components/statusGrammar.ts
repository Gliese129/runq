// statusGrammar — the SINGLE source of truth for status → visual mapping.
//
// Design philosophy U3: every state has exactly one color + icon + variant,
// defined here and consumed everywhere (badges, dots, progress bars, future
// heatmap). No view may hard-code a status color.
//
// Two enums exist BY DESIGN (different abstraction levels):
//   task: pending | running | success | failed | killed
//         (+ cancelling — a frontend-only transitional state, never persisted)
//   job:  pending | running | done | paused
//
// Semantics to remember:
//   - job "done" only means "all tasks reached a terminal state". A job whose
//     tasks ALL failed is still "done" — therefore done is NEUTRAL, never
//     green. Quality is expressed by the task counts shown next to it.
//   - running and success share green but MUST differ in ≥2 non-color
//     channels (hollow+spin vs solid+check) for colorblind / reduced-motion
//     users.
//   - killed is a human decision, not a failure → grey, so that red in a
//     table scan means "actual failure" only.

export interface StatusStyle {
  /** color for vuetify components (theme name or css color) */
  color: string
  /** css color for dots / charts */
  css: string
  /** mdi icon */
  icon: string
  /** chip variant: outlined = transient state, tonal = terminal state */
  variant: 'outlined' | 'tonal'
  /** spinning icon / pulsing dot (auto-disabled by prefers-reduced-motion) */
  animated?: boolean
  /** hollow dot (ring) — running vs success beyond color */
  hollow?: boolean
}

const GREY = '#94A3B8' // slate-400
const GREY_DARK = '#64748B' // slate-500
const BLUE_GREY = '#607D8B' // blue-grey-500
const GREEN = 'rgb(var(--v-theme-success))'
const RED = 'rgb(var(--v-theme-error))'
const AMBER = 'rgb(var(--v-theme-warning))'

const UNKNOWN: StatusStyle = { color: GREY, css: GREY, icon: 'mdi-help-circle-outline', variant: 'outlined' }

export const taskStatusStyles: Record<string, StatusStyle> = {
  pending: { color: GREY, css: GREY, icon: 'mdi-clock-outline', variant: 'outlined' },
  running: { color: 'success', css: GREEN, icon: 'mdi-loading', variant: 'outlined', animated: true, hollow: true },
  success: { color: 'success', css: GREEN, icon: 'mdi-check', variant: 'tonal' },
  failed: { color: 'error', css: RED, icon: 'mdi-alert-circle', variant: 'tonal' },
  killed: { color: GREY_DARK, css: GREY_DARK, icon: 'mdi-stop', variant: 'tonal' },
  // Frontend-only: shown between "kill requested" and the next poll/event
  // confirming it. Never persisted (core philosophy #1).
  cancelling: { color: GREY, css: GREY, icon: 'mdi-loading', variant: 'outlined', animated: true },
}

export const jobStatusStyles: Record<string, StatusStyle> = {
  pending: { color: GREY, css: GREY, icon: 'mdi-clock-outline', variant: 'outlined' },
  running: { color: 'success', css: GREEN, icon: 'mdi-loading', variant: 'outlined', animated: true, hollow: true },
  done: { color: BLUE_GREY, css: BLUE_GREY, icon: 'mdi-flag-checkered', variant: 'tonal' },
  paused: { color: 'warning', css: AMBER, icon: 'mdi-pause', variant: 'tonal' },
}

export type StatusKind = 'task' | 'job'

export function statusStyle(kind: StatusKind, status: string): StatusStyle {
  const table = kind === 'job' ? jobStatusStyles : taskStatusStyles
  return table[status] ?? UNKNOWN
}
