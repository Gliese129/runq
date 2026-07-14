// statusGrammar — the SINGLE source of truth for status → visual mapping.
//
// Design philosophy U3: every state has exactly one color + icon + variant,
// defined here and consumed everywhere (badges, dots, progress bars, future
// heatmap). No view may hard-code a status color.
//
// Two enums exist BY DESIGN (different abstraction levels):
//   task: pending | running | success | failed | killed
//         (+ cancelling — a frontend-only transitional state, never persisted)
//   job:  pending | running | paused | done | failed | partial | killed
//         (terminal split mirrors store.TerminalJobStatus on the backend)
//
// Palette (user-chosen: the CI convention — read a job row like a CI
// pipeline):
//   success/done = success — green means "it worked"
//   failed       = error   — red means "it broke"
//   running      = info    — blue + spin means "alive right now"
//   pending      = slate grey (raw hex, NOT a vuetify semantic color —
//                  waiting is the absence of an outcome, it earns no voice)
//   killed       = warning — a human decision, distinct from failure red
//   partial      = warning — mixed outcome, needs a human look
//   paused       = warning — human intervention; killed/partial/paused all
//                  share amber and are separated by icon (stop/alert/pause)
//
// Running is a solid dot like everything else — the spin alone separates
// live from terminal; hollow is reserved (unused) should two states share
// both color and motion.

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
  /** hollow dot (ring) — extra channel if two states ever share a color */
  hollow?: boolean
}

const SLATE = '#94A3B8' // slate-400: pending + unknown, outside the theme
const GREEN = 'rgb(var(--v-theme-success))'
const RED = 'rgb(var(--v-theme-error))'
const AMBER = 'rgb(var(--v-theme-warning))'
const BLUE = 'rgb(var(--v-theme-info))'

/** Empty-track tint for progress bars — derived from the pending slate. */
export const TRACK_CSS = 'rgba(148, 163, 184, 0.25)'

const UNKNOWN: StatusStyle = { color: SLATE, css: SLATE, icon: 'mdi-help-circle-outline', variant: 'outlined' }

export const taskStatusStyles: Record<string, StatusStyle> = {
  pending: { color: SLATE, css: SLATE, icon: 'mdi-clock-outline', variant: 'outlined' },
  running: { color: 'info', css: BLUE, icon: 'mdi-loading', variant: 'outlined', animated: true },
  success: { color: 'success', css: GREEN, icon: 'mdi-check', variant: 'tonal' },
  failed: { color: 'error', css: RED, icon: 'mdi-alert-circle', variant: 'tonal' },
  killed: { color: 'warning', css: AMBER, icon: 'mdi-stop', variant: 'tonal' },
  // Frontend-only: shown between "kill requested" and the next poll/event
  // confirming it. Never persisted (core philosophy #1).
  cancelling: { color: 'warning', css: AMBER, icon: 'mdi-loading', variant: 'outlined', animated: true },
}

export const jobStatusStyles: Record<string, StatusStyle> = {
  pending: { color: SLATE, css: SLATE, icon: 'mdi-clock-outline', variant: 'outlined' },
  running: { color: 'info', css: BLUE, icon: 'mdi-loading', variant: 'outlined', animated: true },
  done: { color: 'success', css: GREEN, icon: 'mdi-flag-checkered', variant: 'tonal' },
  failed: { color: 'error', css: RED, icon: 'mdi-alert-circle', variant: 'tonal' },
  partial: { color: 'warning', css: AMBER, icon: 'mdi-alert-circle-outline', variant: 'tonal' },
  killed: { color: 'warning', css: AMBER, icon: 'mdi-stop', variant: 'tonal' },
  paused: { color: 'warning', css: AMBER, icon: 'mdi-pause', variant: 'tonal' },
}

export type StatusKind = 'task' | 'job'

export function statusStyle(kind: StatusKind, status: string): StatusStyle {
  const table = kind === 'job' ? jobStatusStyles : taskStatusStyles
  return table[status] ?? UNKNOWN
}
