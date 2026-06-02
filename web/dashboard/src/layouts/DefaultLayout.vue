<template>
  <v-app>
    <!-- Sidebar rail navigation -->
    <v-navigation-drawer
      rail
      permanent
      color="surface"
      style="border-right: 1px solid rgba(0,0,0,0.06)"
    >
      <div class="d-flex flex-column align-center py-3">
        <!-- Logo -->
        <router-link :to="{ name: 'overview' }" class="text-decoration-none mb-4">
          <RunqLogo :size="36" />
        </router-link>
      </div>

      <!-- Nav items -->
      <div class="d-flex flex-column align-center ga-1 px-1">
        <v-tooltip v-for="item in navItems" :key="item.name" :text="item.label" location="end">
          <template #activator="{ props: tp }">
            <v-btn
              v-bind="tp"
              icon
              size="small"
              :variant="isActive(item.name) ? 'tonal' : 'text'"
              :color="isActive(item.name) ? 'primary' : undefined"
              :to="item.to"
              class="mb-1"
            >
              <v-icon size="20">{{ item.icon }}</v-icon>
            </v-btn>
          </template>
        </v-tooltip>
      </div>

      <v-spacer />

      <!-- Bottom: connection + settings -->
      <template #append>
        <div class="d-flex flex-column align-center ga-1 pb-3 px-1">
          <!-- Connection indicator -->
          <v-tooltip :text="conn.connected.value ? 'Connected' : conn.lastError.value" location="end">
            <template #activator="{ props: tp }">
              <div v-bind="tp" class="d-flex justify-center" style="width: 100%">
                <div
                  class="rounded-circle"
                  :style="{
                    width: '8px',
                    height: '8px',
                    background: conn.connected.value
                      ? 'rgb(var(--v-theme-success))'
                      : 'rgb(var(--v-theme-error))',
                    transition: 'background 0.3s ease',
                  }"
                  :class="{ 'pulse-dot': !conn.connected.value }"
                />
              </div>
            </template>
          </v-tooltip>

          <v-tooltip :text="t('nav.settings')" location="end">
            <template #activator="{ props: tp }">
              <v-btn
                v-bind="tp"
                icon
                size="small"
                :variant="isActive('settings') ? 'tonal' : 'text'"
                :color="isActive('settings') ? 'primary' : undefined"
                :to="{ name: 'settings' }"
              >
                <v-icon size="20">mdi-cog-outline</v-icon>
              </v-btn>
            </template>
          </v-tooltip>
        </div>
      </template>
    </v-navigation-drawer>

    <!-- Top bar (minimal — just context info) -->
    <v-app-bar elevation="0" color="transparent" style="border-bottom: 1px solid rgba(0,0,0,0.04)">
      <v-app-bar-title class="text-body-2 text-on-surface-variant d-flex align-center ga-2">
        <span class="font-weight-bold text-on-surface">{{ pageTitle }}</span>
        <v-chip v-if="config.loaded" size="x-small" variant="tonal" color="primary" label>
          {{ config.mode }}
        </v-chip>
      </v-app-bar-title>

      <template #append>
        <!-- GPU chip -->
        <v-chip
          v-if="config.features.gpu_map && gpu.totalCount > 0"
          size="small"
          variant="tonal"
          :color="gpu.freeCount > 0 ? 'success' : 'warning'"
          class="mr-2"
        >
          <v-icon start size="14">mdi-memory</v-icon>
          {{ gpu.freeCount }}/{{ gpu.totalCount }}
        </v-chip>

        <!-- Language selector -->
        <v-menu>
          <template #activator="{ props: mp }">
            <v-btn v-bind="mp" variant="text" size="small" class="mr-1 text-caption">
              {{ currentLangLabel }}
              <v-icon end size="14">mdi-chevron-down</v-icon>
            </v-btn>
          </template>
          <v-list density="compact" min-width="120">
            <v-list-item
              v-for="lang in locales"
              :key="lang.value"
              :active="settings.locale === lang.value"
              @click="switchLocale(lang.value)"
            >
              <v-list-item-title class="text-body-2">{{ lang.label }}</v-list-item-title>
            </v-list-item>
          </v-list>
        </v-menu>

        <!-- Theme toggle -->
        <v-btn
          icon
          size="small"
          variant="text"
          @click="toggleTheme"
        >
          <v-icon size="18">{{ settings.theme === 'dark' ? 'mdi-white-balance-sunny' : 'mdi-moon-waning-crescent' }}</v-icon>
        </v-btn>
      </template>
    </v-app-bar>

    <v-main>
      <v-container fluid class="pa-5 pa-md-8" style="max-width: 1200px">
        <router-view v-slot="{ Component, route }">
          <transition name="fade-slide" mode="out-in">
            <component :is="Component" :key="route.path" />
          </transition>
        </router-view>
      </v-container>
    </v-main>
  </v-app>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { useTheme } from 'vuetify'
import { useConfigStore } from '@/stores/config'
import { useGPUStore } from '@/stores/gpu'
import { useSettingsStore } from '@/stores/settings'
import { useConnection } from '@/composables/useConnection'
import RunqLogo from '@/components/RunqLogo.vue'

const { t, locale } = useI18n()
const route = useRoute()
const theme = useTheme()
const config = useConfigStore()
const gpu = useGPUStore()
const settings = useSettingsStore()
const conn = useConnection()

const locales = [
  { value: 'en', label: 'EN' },
  { value: 'ja', label: 'JA' },
  { value: 'zh-CN', label: 'ZH' },
]

const currentLangLabel = computed(() =>
  locales.find(l => l.value === settings.locale)?.label || 'EN'
)

function switchLocale(val: string) {
  settings.setLocale(val)
  locale.value = val
}

function toggleTheme() {
  const next = settings.theme === 'dark' ? 'light' : 'dark'
  settings.setTheme(next)
  theme.global.name.value = next
}

const navItems = computed(() => [
  { name: 'overview', label: t('nav.overview'), icon: 'mdi-view-dashboard-outline', to: { name: 'overview' } },
  { name: 'submit', label: t('nav.submit'), icon: 'mdi-plus-circle-outline', to: { name: 'submit' } },
  { name: 'about', label: t('nav.about'), icon: 'mdi-information-outline', to: { name: 'about' } },
])

const pageTitle = computed(() => {
  switch (route.name) {
    case 'overview': return t('nav.overview')
    case 'submit': return t('submit.title')
    case 'settings': return t('settings.title')
    case 'about': return t('about.title')
    case 'project': return String(route.params.project || '')
    default: return 'runq'
  }
})

function isActive(name: string): boolean {
  return route.name === name
}

onMounted(async () => {
  try {
    if (!config.loaded) await config.fetchConfig()
    if (config.features.gpu_map) gpu.fetchGPU()
  } catch {
    // connection error tracked globally
  }
})
</script>
