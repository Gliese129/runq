import 'vuetify/styles'
import '@mdi/font/css/materialdesignicons.css'
import { createVuetify } from 'vuetify'

const light = {
  dark: false,
  colors: {
    primary: '#6C63FF',
    'primary-darken-1': '#534AB7',
    secondary: '#2DB88A',
    error: '#E5574F',
    warning: '#F5A623',
    success: '#5CB85C',
    info: '#4A9EF5',
    surface: '#FFFFFF',
    'surface-variant': '#F5F4F0',
    background: '#FAFAF8',
    'on-background': '#2C2C2C',
    'on-surface': '#2C2C2C',
    'on-surface-variant': '#6E6E6E',
  },
}

const dark = {
  dark: true,
  colors: {
    primary: '#B4ADFF',
    'primary-darken-1': '#8B83E0',
    secondary: '#6DDBB2',
    error: '#F09595',
    warning: '#FAC775',
    success: '#8DD879',
    info: '#85B7EB',
    surface: '#242424',
    'surface-variant': '#2E2E2C',
    background: '#1A1A18',
    'on-background': '#E8E8E4',
    'on-surface': '#E8E8E4',
    'on-surface-variant': '#A0A098',
  },
}

export default createVuetify({
  theme: {
    defaultTheme: localStorage.getItem('runq-theme') || 'light',
    themes: { light, dark },
  },
  defaults: {
    VCard: {
      elevation: 1,
      rounded: 'xl',
    },
    VBtn: {
      rounded: 'pill',
      variant: 'text',
    },
    VChip: {
      size: 'small',
      rounded: 'pill',
    },
    VTextField: {
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
      density: 'comfortable',
      hover: true,
    },
    VTab: {
      rounded: 'pill',
    },
    VSnackbar: {
      rounded: 'xl',
      timeout: 4000,
    },
  },
})
