<template>
  <v-app>
    <v-navigation-drawer
      v-model="drawerOpen"
      :rail="collapsed"
      :permanent="!mobile"
      :temporary="mobile"
      color="surface"
      width="240"
      style="border-right: 0.5px solid rgb(var(--v-theme-outline-variant))"
    >
      <div class="d-flex align-center ga-2 pa-3" style="height: 56px">
        <router-link :to="{ name: 'overview' }" :aria-label="t('a11y.home')" class="text-decoration-none d-flex align-center ga-2">
          <RunqLogo :size="28" />
          <span v-if="!collapsed" class="text-body-1 font-weight-bold text-on-surface">runq</span>
        </router-link>
        <v-spacer />
        <v-chip v-if="!collapsed && config.loaded" size="x-small" variant="tonal" color="primary" label>
          {{ config.mode }}
        </v-chip>
      </div>

      <v-divider />

      <div class="pa-2">
        <v-list-item
          v-for="item in navItems"
          :key="item.name"
          :to="item.to"
          :active="isActive(item.name)"
          :title="collapsed ? '' : item.label"
          :aria-label="item.label"
          :prepend-icon="item.icon"
          density="compact"
          rounded="lg"
          class="mb-1"
          color="primary"
        />
      </div>

      <v-divider />

      <div class="pa-2">
        <div v-if="!collapsed" class="text-caption text-on-surface-variant px-2 mb-1">
          {{ t('nav.utils') }}
        </div>
        <v-tooltip v-else :text="t('nav.utils')" location="end">
          <template #activator="{ props: tp }">
            <div v-bind="tp" class="text-center mb-1">
              <v-icon size="16" color="on-surface-variant">mdi-toolbox-outline</v-icon>
            </div>
          </template>
        </v-tooltip>
        <v-list-item
          :to="{ name: 'log-viewer' }"
          :active="isActive('log-viewer')"
          :title="collapsed ? '' : t('nav.log_viewer')"
          :aria-label="t('nav.log_viewer')"
          prepend-icon="mdi-text-box-search-outline"
          density="compact"
          rounded="lg"
          class="mb-1"
          color="primary"
        />
      </div>

      <v-divider />

      <div class="pa-2 flex-grow-1 overflow-y-auto">
        <div v-if="!collapsed" class="text-caption text-on-surface-variant px-2 mb-1 d-flex align-center justify-space-between">
          Projects
          <v-btn icon size="x-small" variant="text" @click="refreshShellData">
            <v-icon size="12">mdi-refresh</v-icon>
          </v-btn>
        </div>
        <v-tooltip v-else text="Projects" location="end">
          <template #activator="{ props: tp }">
            <div v-bind="tp" class="text-center mb-1">
              <v-icon size="16" color="on-surface-variant">mdi-folder-multiple-outline</v-icon>
            </div>
          </template>
        </v-tooltip>

        <v-list-item
          v-for="proj in projects.visible"
          :key="proj.name"
          :to="{ name: 'project', params: { project: proj.name } }"
          :active="projects.selected === proj.name"
          :aria-label="proj.name"
          density="compact"
          rounded="lg"
          class="mb-1"
          color="primary"
          @click="projects.select(proj.name)"
        >
          <template #prepend>
            <div class="status-dot mr-2" :style="{ background: projectColor(proj.name) }" />
          </template>
          <v-list-item-title v-if="!collapsed" class="text-body-2">{{ proj.name }}</v-list-item-title>
          <template v-if="!collapsed" #append>
            <span class="text-caption text-on-surface-variant">{{ proj.job_count }}</span>
          </template>
        </v-list-item>

        <div v-if="projects.visible.length === 0 && !projects.loading && !collapsed" class="text-caption text-on-surface-variant text-center pa-3">
          No projects yet
        </div>
      </div>

      <template #append>
        <v-divider />
        <div class="pa-2">
          <v-list-item
            :to="{ name: 'settings' }"
            :active="isActive('settings')"
            :title="collapsed ? '' : t('nav.settings')"
            :aria-label="t('nav.settings')"
            prepend-icon="mdi-cog-outline"
            density="compact"
            rounded="lg"
            color="primary"
          />
          <v-btn
            block
            variant="text"
            density="comfortable"
            rounded="lg"
            class="nav-btn mb-1"
            :class="collapsed ? 'justify-center' : 'justify-start px-3'"
            :prepend-icon="settings.theme === 'dark' ? 'mdi-white-balance-sunny' : 'mdi-moon-waning-crescent'"
            :aria-label="settings.theme === 'dark' ? 'Light mode' : 'Dark mode'"
            @click="toggleTheme"
          >
            <span v-if="!collapsed" class="text-body-2">{{ settings.theme === 'dark' ? 'Light mode' : 'Dark mode' }}</span>
          </v-btn>
          <v-btn
            v-if="!mobile"
            block
            variant="text"
            density="comfortable"
            rounded="lg"
            class="nav-btn mb-1"
            :class="collapsed ? 'justify-center' : 'justify-start px-3'"
            :prepend-icon="collapsed ? 'mdi-chevron-right' : 'mdi-chevron-left'"
            :aria-label="collapsed ? 'Expand sidebar' : 'Collapse sidebar'"
            @click="toggleCollapse"
          >
            <span v-if="!collapsed" class="text-body-2">Collapse</span>
          </v-btn>
          <div class="d-flex align-center ga-2 px-3 py-1">
            <div class="status-dot" :class="conn.connected.value ? 'status-dot--completed' : 'status-dot--failed'" />
            <span v-if="!collapsed" class="text-caption text-on-surface-variant">
              {{ t(conn.statusKey.value) }}
            </span>
          </div>
        </div>
      </template>
    </v-navigation-drawer>

    <v-app-bar elevation="0" color="transparent" density="compact" style="border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant))">
      <v-btn v-if="mobile" icon size="small" variant="text" @click="drawerOpen = !drawerOpen">
        <v-icon>mdi-menu</v-icon>
      </v-btn>
      <v-breadcrumbs v-if="breadcrumbs.length > 0" :items="breadcrumbs" density="compact" class="text-body-2 pa-0 ml-2">
        <template #divider><v-icon size="12">mdi-chevron-right</v-icon></template>
      </v-breadcrumbs>
      <v-spacer />
      <v-chip
        v-if="config.caps.gpu_map && gpu.totalCount > 0"
        size="small" variant="tonal"
        :color="gpu.freeCount > 0 ? 'success' : 'warning'"
        class="mr-2"
      >
        <v-icon start size="14">mdi-memory</v-icon>
        {{ gpu.freeCount }}/{{ gpu.totalCount }}
      </v-chip>
    </v-app-bar>

    <v-main>
      <v-container fluid class="pa-4 pa-md-6" style="max-width: 1200px">
        <router-view v-slot="{ Component, route: r }">
          <transition name="fade-slide" mode="out-in">
            <component :is="Component" :key="r.path" />
          </transition>
        </router-view>
      </v-container>
    </v-main>
  </v-app>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { useTheme, useDisplay } from 'vuetify'
import { useConfigStore } from '@/stores/config'
import { useGPUStore } from '@/stores/gpu'
import { useSettingsStore } from '@/stores/settings'
import { useProjectStore } from '@/stores/projects'
import { useJobsStore } from '@/stores/jobs'
import { useConnection } from '@/composables/useConnection'
import RunqLogo from '@/components/RunqLogo.vue'

const { t } = useI18n()
const route = useRoute()
const theme = useTheme()
const { mobile } = useDisplay()
const config = useConfigStore()
const gpu = useGPUStore()
const settings = useSettingsStore()
const projects = useProjectStore()
const jobs = useJobsStore()
const conn = useConnection()

const drawerOpen = ref(true)
const collapsed = ref(localStorage.getItem('runq-sidebar-collapsed') === 'true')

function toggleCollapse() {
  collapsed.value = !collapsed.value
  localStorage.setItem('runq-sidebar-collapsed', String(collapsed.value))
}

function toggleTheme() {
  const next = settings.theme === 'dark' ? 'light' : 'dark'
  settings.setTheme(next)
  theme.global.name.value = next
}

const navItems = computed(() => [
  { name: 'overview', label: t('nav.overview'), icon: 'mdi-view-dashboard-outline', to: { name: 'overview' } },
  { name: 'submit', label: t('nav.submit'), icon: 'mdi-plus-circle-outline', to: { name: 'submit' } },
])

function isActive(name: string): boolean {
  return route.name === name
}

const breadcrumbs = computed(() => {
  const items: { title: string; to?: object; disabled?: boolean }[] = []
  if (route.params.project) {
    items.push({ title: String(route.params.project), to: { name: 'project', params: { project: route.params.project } } })
  }
  if (route.params.jobId) {
    items.push({
      title: String(route.params.jobId).slice(0, 8),
      to: route.params.taskId ? { name: 'job-detail', params: { project: route.params.project, jobId: route.params.jobId } } : undefined,
      disabled: !route.params.taskId,
    })
  }
  if (route.params.taskId) {
    items.push({ title: String(route.params.taskId).slice(0, 8), disabled: true })
  }
  return items
})

const PROJECT_COLORS = ['#1E40AF', '#16A34A', '#D97706', '#DC2626', '#7C3AED', '#0891B2', '#DB2777', '#65A30D']
function projectColor(name: string): string {
  let hash = 0
  for (let i = 0; i < name.length; i++) hash = ((hash << 5) - hash + name.charCodeAt(i)) | 0
  return PROJECT_COLORS[Math.abs(hash) % PROJECT_COLORS.length]
}

function refreshShellData() {
  projects.fetch()
  jobs.fetchJobs()
}

onMounted(async () => {
  try {
    if (!config.loaded) await config.fetchConfig()
    if (config.caps.gpu_map) gpu.fetchGPU()
    refreshShellData()
  } catch {}
})
</script>

<style scoped>
/* Theme / collapse toggles are real buttons; match the nav list-item look. */
.nav-btn {
  text-transform: none;
  letter-spacing: 0;
}
/* Center the icon cleanly when the rail is collapsed (drop prepend gap). */
.nav-btn.justify-center :deep(.v-btn__prepend) {
  margin-inline: 0;
}
</style>
