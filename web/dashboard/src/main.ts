import { createApp } from 'vue'
import App from './App.vue'
import vuetify from '@/plugins/vuetify'
import i18n from '@/plugins/i18n'
import pinia from '@/plugins/pinia'
import router from '@/plugins/router'
import '@/assets/main.css'

const app = createApp(App)
app.use(vuetify)
app.use(i18n)
app.use(pinia)
app.use(router)
app.mount('#app')
