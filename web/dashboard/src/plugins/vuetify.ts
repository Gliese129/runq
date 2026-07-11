import 'vuetify/styles'
import { h } from 'vue'
import { createVuetify, type IconSet, type IconProps } from 'vuetify'
import { aliases, mdi } from 'vuetify/iconsets/mdi-svg'
import { MDI_PATHS } from './mdiPaths'

// ── Icons: SVG paths, resolved by name ──
// @mdi/font shipped all 7000+ glyphs (~400KB woff2 + ~90KB gzip CSS) for
// the ~90 we use. Templates keep writing plain `mdi-*` strings; this set
// resolves them through the generated MDI_PATHS map (yarn gen:icons).
// Unknown names render empty — the generator fails CI-side first.
const mdiSvgByName: IconSet = {
  component: (props: IconProps) =>
    h(mdi.component, { ...props, icon: MDI_PATHS[props.icon as string] ?? '' }),
}

// ──────────────────────────────────────────────
// Theme A (Flat Professional) + B (Terminal Data)
// Light: A-base shell, B-accented data areas
// Dark:  full B-style
// ──────────────────────────────────────────────

const light = {
  dark: false,
  colors: {
    primary: '#1E40AF',         // Blue 800 — nav, links, active
    'primary-darken-1': '#1E3A8A',
    secondary: '#3B82F6',       // Blue 500 — lighter accent
    error: '#DC2626',           // Red 600
    warning: '#D97706',         // Amber 600
    success: '#16A34A',         // Green 600
    info: '#0891B2',            // Cyan 600 — must stay distinct from primary (pending vs done)
    surface: '#FFFFFF',
    'surface-variant': '#F1F5F9', // Slate 100
    background: '#F8FAFC',       // Slate 50
    'on-background': '#0F172A',  // Slate 900
    'on-surface': '#0F172A',
    'on-surface-variant': '#64748B', // Slate 500
    'outline-variant': '#E2E8F0',    // Slate 200
  },
}

const dark = {
  dark: true,
  colors: {
    primary: '#60A5FA',         // Blue 400
    'primary-darken-1': '#3B82F6',
    secondary: '#93C5FD',       // Blue 300
    error: '#F87171',           // Red 400
    warning: '#FBBF24',         // Amber 400
    success: '#4ADE80',         // Green 400
    info: '#22D3EE',            // Cyan 400 — must stay distinct from primary (pending vs done)
    surface: '#0F172A',         // Slate 900
    'surface-variant': '#1E293B', // Slate 800
    background: '#020617',       // Slate 950
    'on-background': '#F1F5F9',  // Slate 100
    'on-surface': '#F1F5F9',
    'on-surface-variant': '#94A3B8', // Slate 400
    'outline-variant': '#334155',    // Slate 700
  },
}

export default createVuetify({
  icons: {
    defaultSet: 'mdi',
    aliases, // vuetify-internal $close/$menu/... → svg paths
    sets: { mdi: mdiSvgByName },
  },
  theme: {
    defaultTheme: localStorage.getItem('runq-theme') || 'light',
    themes: { light, dark },
  },
  defaults: {
    // ── Global: flat, compact, no Material effects ──
    global: {
      ripple: false,
    },
    VCard: {
      elevation: 0,
      rounded: 'lg',       // 8px — 6px via CSS override below
      border: true,         // use border instead of shadow
    },
    VBtn: {
      rounded: 'lg',
      variant: 'text',
    },
    VChip: {
      size: 'small',
      rounded: 'lg',
    },
    VTextField: {
      variant: 'outlined',
      density: 'compact',
      rounded: 'lg',
      hideDetails: 'auto',
    },
    VTextarea: {
      variant: 'outlined',
      density: 'compact',
      rounded: 'lg',
    },
    VSelect: {
      variant: 'outlined',
      density: 'compact',
      rounded: 'lg',
    },
    VSwitch: {
      color: 'primary',
      density: 'compact',
      inset: true,
    },
    VDataTable: {
      density: 'compact',
      hover: true,
    },
    VList: {
      density: 'compact',
    },
    VExpansionPanel: {
      elevation: 0,
    },
    VTab: {
      rounded: 'lg',
    },
    VSnackbar: {
      rounded: 'lg',
      timeout: 3000,
    },
    VSheet: {
      rounded: 'lg',
    },
    VNavigationDrawer: {
      elevation: 0,
    },
  },
})
