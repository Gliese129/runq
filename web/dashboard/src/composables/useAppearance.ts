// Appearance levers (RQ2-2) — three levers that reshape the whole surface
// through CSS custom properties + root class hooks (kit tweaks.jsx):
//
//   density: the data's breathing room   (compact | regular | comfy)
//   surface: what a "card" means         (hairline | elevated | grid)
//   accent:  the brand hue               (4 curated light/dark pairs)
//
// The lever CSS lives statically in assets/main.css under `.appearance-*`
// classes; this composable only flips root classes, retunes the vuetify
// theme colors (accent), and persists state. No component takes a lever
// prop — the whole app reskins at once.
//
// Persistence (RQ2-1 alignment #12): ui.json is the roaming home — the
// backend stores it as an opaque blob next to config.yaml, so preferences
// follow the daemon across machines. localStorage is the offline
// fallback/cache: applied instantly on boot, superseded when the server
// copy arrives, still functional when the daemon is down.

import { ref, watch } from 'vue'
import vuetify from '@/plugins/vuetify'
import { uiApi } from '@/apis/ui'

export type Density = 'compact' | 'regular' | 'comfy'
export type Surface = 'hairline' | 'elevated' | 'grid'

export interface Appearance {
  density: Density
  surface: Surface
  accent: string
}

/** Curated accent pairs (kit tweaks.jsx ACCENTS) — never a free picker.
 *  Key = light primary; dark/darken/secondary keep both themes coherent. */
export const ACCENTS: Record<string, { name: string; dark: string; darken: string; secondary: string }> = {
  '#1A3FA8': { name: 'Indigo', dark: '#5B9CF7', darken: '#182F7E', secondary: '#3674EE' },
  '#0F766E': { name: 'Teal', dark: '#2DD4BF', darken: '#115E59', secondary: '#14B8A6' },
  '#6D28D9': { name: 'Violet', dark: '#A78BFA', darken: '#5B21B6', secondary: '#8B5CF6' },
  '#B45309': { name: 'Ember', dark: '#FBBF24', darken: '#92400E', secondary: '#D97706' },
}

// NOTE: the kit's RUNQ_TWEAK_DEFAULTS uses density 'compact'; the app
// defaults to 'regular' because that IS today's baseline look (the kit's
// 'regular' = these token values) — a token-port ticket must not silently
// retune every existing screen. Flip here if the kit default is adopted.
export const APPEARANCE_DEFAULTS: Appearance = {
  density: 'regular',
  surface: 'hairline',
  accent: '#1A3FA8',
}

const STORAGE_KEY = 'runq-appearance'

const DENSITIES: Density[] = ['compact', 'regular', 'comfy']
const SURFACES: Surface[] = ['hairline', 'elevated', 'grid']

/** Validate an untrusted appearance blob key-by-key (ui.json is opaque to
 *  the backend — a hand-edited file must not wedge the UI). */
function sanitize(raw: unknown): Partial<Appearance> {
  if (typeof raw !== 'object' || raw === null) return {}
  const r = raw as Record<string, unknown>
  const out: Partial<Appearance> = {}
  if (DENSITIES.includes(r.density as Density)) out.density = r.density as Density
  if (SURFACES.includes(r.surface as Surface)) out.surface = r.surface as Surface
  if (typeof r.accent === 'string' && r.accent in ACCENTS) out.accent = r.accent
  return out
}

function readLocal(): Appearance {
  try {
    return { ...APPEARANCE_DEFAULTS, ...sanitize(JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}')) }
  } catch {
    return { ...APPEARANCE_DEFAULTS }
  }
}

const initial = readLocal()
const density = ref<Density>(initial.density)
const surface = ref<Surface>(initial.surface)
const accent = ref<string>(initial.accent)

function current(): Appearance {
  return { density: density.value, surface: surface.value, accent: accent.value }
}

function apply() {
  const el = document.documentElement
  el.classList.remove(...Array.from(el.classList).filter((c) => c.startsWith('appearance-')))
  el.classList.add(`appearance-density-${density.value}`, `appearance-surface-${surface.value}`)

  // Accent retunes the vuetify themes directly — the compiled
  // --v-theme-primary vars are RGB triplets, so a plain CSS override
  // can't express them; the theme API recompiles both modes coherently.
  const a = ACCENTS[accent.value] ?? ACCENTS[APPEARANCE_DEFAULTS.accent]
  const light = vuetify.theme.themes.value.light.colors
  light.primary = accent.value
  light['primary-darken-1'] = a.darken
  light.secondary = a.secondary
  const dark = vuetify.theme.themes.value.dark.colors
  dark.primary = a.dark
  dark['primary-darken-1'] = accent.value
  dark.secondary = a.secondary
}

// ── Persistence: localStorage immediately, ui.json debounced ──

let pushTimer: ReturnType<typeof setTimeout> | undefined
let adoptingRemote = false

async function pushRemote() {
  try {
    // PUT replaces the whole document — read-merge-write so grouping and
    // future keys survive. No CAS by design (RQ2-1 #12): last write wins,
    // a lost preference write costs nothing.
    const doc = (await uiApi.get()) ?? {}
    await uiApi.put({ ...doc, appearance: current() })
  } catch {
    // Offline / daemon down: localStorage already holds it; the next
    // successful change pushes the merged state.
  }
}

watch([density, surface, accent], () => {
  apply()
  localStorage.setItem(STORAGE_KEY, JSON.stringify(current()))
  if (adoptingRemote) return // echoing the server's own copy back is noise
  clearTimeout(pushTimer)
  pushTimer = setTimeout(pushRemote, 800)
})

/** Boot: apply the local copy instantly, then adopt the roaming copy. */
export async function initAppearance() {
  apply()
  try {
    const doc = await uiApi.get()
    const remote = sanitize((doc as Record<string, unknown> | null)?.appearance)
    if (Object.keys(remote).length > 0) {
      adoptingRemote = true
      if (remote.density) density.value = remote.density
      if (remote.surface) surface.value = remote.surface
      if (remote.accent) accent.value = remote.accent
      // The watcher runs post-flush; release the flag the same way.
      setTimeout(() => {
        adoptingRemote = false
      })
    }
  } catch {
    // Offline boot: the localStorage copy already applied.
  }
}

export function useAppearance() {
  return { density, surface, accent }
}
