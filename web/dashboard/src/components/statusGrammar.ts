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
// Palette (user-chosen, jobs and tasks share it):
//   done/success = primary   — terminal "completed", calm and final
//   running      = success   — green means "alive and working"
//   failed       = error
//   pending      = info
//   killed       = warning   — a human decision, distinct from failure red
//
// Non-color channels still matter: running stays hollow+spinning vs the
// solid terminal states (colorblind / reduced-motion users).

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

const GREY = '#94A3B8' // slate-400 (unknown only)
const PRIMARY = 'rgb(var(--v-theme-primary))'
const GREEN = 'rgb(var(--v-theme-success))'
const RED = 'rgb(var(--v-theme-error))'
const AMBER = 'rgb(var(--v-theme-warning))'
const INFO = 'rgb(var(--v-theme-info))'

const UNKNOWN: StatusStyle = { color: GREY, css: GREY, icon: 'mdi-help-circle-outline', variant: 'outlined' }

export const taskStatusStyles: Record<string, StatusStyle> = {
  pending: { color: 'info', css: INFO, icon: 'mdi-clock-outline', variant: 'outlined' },
  running: { color: 'success', css: GREEN, icon: 'mdi-loading', variant: 'outlined', animated: true, hollow: true },
  success: { color: 'primary', css: PRIMARY, icon: 'mdi-check', variant: 'tonal' },
  failed: { color: 'error', css: RED, icon: 'mdi-alert-circle', variant: 'tonal' },
  killed: { color: 'warning', css: AMBER, icon: 'mdi-stop', variant: 'tonal' },
  // Frontend-only: shown between "kill requested" and the next poll/event
  // confirming it. Never persisted (core philosophy #1).
  cancelling: { color: 'warning', css: AMBER, icon: 'mdi-loading', variant: 'outlined', animated: true },
}

export const jobStatusStyles: Record<string, StatusStyle> = {
  pending: { color: 'info', css: INFO, icon: 'mdi-clock-outline', variant: 'outlined' },
  running: { color: 'success', css: GREEN, icon: 'mdi-loading', variant: 'outlined', animated: true, hollow: true },
  done: { color: 'primary', css: PRIMARY, icon: 'mdi-flag-checkered', variant: 'tonal' },
  paused: { color: 'warning', css: AMBER, icon: 'mdi-pause', variant: 'tonal' },
}

export type StatusKind = 'task' | 'job'

export function statusStyle(kind: StatusKind, status: string): StatusStyle {
  const table = kind === 'job' ? jobStatusStyles : taskStatusStyles
  return table[status] ?? UNKNOWN
}
