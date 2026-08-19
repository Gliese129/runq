import { createApp } from 'vue'
import { VueQueryPlugin } from '@tanstack/vue-query'
import App from './App.vue'
import vuetify from '@/plugins/vuetify'
import i18n from '@/plugins/i18n'
import pinia from '@/plugins/pinia'
import router from '@/plugins/router'
import { queryClient } from '@/queries/client'
import { initAppearance } from '@/composables/useAppearance'
import '@/assets/main.css'

// Appearance levers: local copy applies synchronously (no flash of default
// look), the roaming ui.json copy is adopted when it arrives.
void initAppearance()

const app = createApp(App)
app.use(vuetify)
app.use(i18n)
app.use(pinia)
app.use(router)
// Defaults live on the shared instance (queries/client.ts) so non-setup
// callers (pinia stores) invalidate through the SAME cache.
app.use(VueQueryPlugin, { queryClient })
app.mount('#app')
