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
        <router-link
          :to="{ name: 'overview' }"
          :aria-label="t('a11y.home')"
          class="text-decoration-none d-flex align-center ga-2"
        >
          <RunqLogo :size="28" />
          <span v-if="!collapsed" class="text-body-1 font-weight-bold text-on-surface">runq</span>
        </router-link>
        <v-spacer />
        <v-chip
          v-if="!collapsed && config.loaded"
          size="x-small"
          variant="tonal"
          color="primary"
          label
        >
          {{ config.targetLabel }}
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
        <div
          v-if="!collapsed"
          class="text-caption text-on-surface-variant px-2 mb-1 d-flex align-center justify-space-between"
        >
          {{ t('overview.projects') }}
          <v-btn
            icon
            size="x-small"
            variant="text"
            :aria-label="t('common.refresh')"
            :title="t('common.refresh')"
            @click="refreshShellData"
          >
            <v-icon size="12">mdi-refresh</v-icon>
          </v-btn>
        </div>
        <v-tooltip v-else :text="t('overview.projects')" location="end">
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
          <v-list-item-title v-if="!collapsed" class="text-body-2">{{
            proj.name
          }}</v-list-item-title>
          <template v-if="!collapsed" #append>
            <span class="text-caption text-on-surface-variant">{{ proj.job_count }}</span>
          </template>
        </v-list-item>

        <div
          v-if="projects.visible.length === 0 && !projects.loading && !collapsed"
          class="text-caption text-on-surface-variant text-center pa-3"
        >
          {{ t('overview.no_projects') }}
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
            :prepend-icon="
              settings.theme === 'dark' ? 'mdi-white-balance-sunny' : 'mdi-moon-waning-crescent'
            "
            :aria-label="settings.theme === 'dark' ? t('layout.light_mode') : t('layout.dark_mode')"
            @click="toggleTheme"
          >
            <span v-if="!collapsed" class="text-body-2">{{
              settings.theme === 'dark' ? t('layout.light_mode') : t('layout.dark_mode')
            }}</span>
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
            :aria-label="collapsed ? t('layout.expand_sidebar') : t('layout.collapse_sidebar')"
            @click="toggleCollapse"
          >
            <span v-if="!collapsed" class="text-body-2">{{ t('common.collapse') }}</span>
          </v-btn>
          <div class="d-flex align-center ga-2 px-3 py-1">
            <div
              class="status-dot"
              :class="conn.connected.value ? 'status-dot--completed' : 'status-dot--failed'"
            />
            <span v-if="!collapsed" class="text-caption text-on-surface-variant">
              {{ t(conn.statusKey.value) }}
            </span>
          </div>
        </div>
      </template>
    </v-navigation-drawer>

    <v-app-bar
      elevation="0"
      color="transparent"
      density="compact"
      style="border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant))"
    >
      <v-btn
        v-if="mobile"
        icon
        size="small"
        variant="text"
        :aria-label="t('a11y.open_menu')"
        @click="drawerOpen = !drawerOpen"
      >
        <v-icon>mdi-menu</v-icon>
      </v-btn>
      <v-breadcrumbs
        v-if="breadcrumbs.length > 0"
        :items="breadcrumbs"
        density="compact"
        class="text-body-2 pa-0 ml-2"
      >
        <template #divider><v-icon size="12">mdi-chevron-right</v-icon></template>
      </v-breadcrumbs>
      <v-spacer />
      <v-chip
        v-if="config.caps.gpu_map && gpuTotal > 0"
        size="small"
        variant="tonal"
        :color="gpuFree > 0 ? 'success' : 'warning'"
        class="mr-2"
      >
        <v-icon start size="14">mdi-memory</v-icon>
        {{ gpuFree }}/{{ gpuTotal }}
      </v-chip>
    </v-app-bar>

    <v-main>
      <!-- RQ-74: reconnect mode — the page never silently dies. Cached data
           stays visible underneath; this banner says why nothing updates
           and disappears on its own when the health probe reconnects. -->
      <v-expand-transition>
        <div
          v-if="!conn.connected.value"
          class="reconnect-banner d-flex align-center ga-2 px-4 py-2"
        >
          <v-progress-circular indeterminate size="14" width="2" />
          <span class="text-body-2">{{ t('statusbar.reconnect_banner') }}</span>
        </div>
      </v-expand-transition>
      <v-container fluid class="pa-4 pa-md-6" style="max-width: 1200px">
        <!-- No route transition ON PURPOSE: mode="out-in" delayed the new
             view until the old one's leave finished, and a poll-driven
             re-render inside that window could stall afterLeave — the old
             page (even the wrong component type) stayed on screen for
             seconds under the new URL (Codex finding, RQ-49). -->
        <router-view v-slot="{ Component, route: r }">
          <component :is="Component" :key="r.path" />
        </router-view>
      </v-container>
    </v-main>

    <!-- RQ-74: VSCode-style status bar — connection, targets, forwards,
         daemon identity. The one place that always answers "can I trust
         what this page shows right now". -->
    <StatusBar />
  </v-app>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { useTheme, useDisplay } from 'vuetify'
import { useQueryClient } from '@tanstack/vue-query'
import { useConfigStore } from '@/stores/config'
import { useSettingsStore } from '@/stores/settings'
import { useProjectStore } from '@/stores/projects'
import { useConnection } from '@/composables/useConnection'
import { useGpuQuery } from '@/queries/useGpuQuery'
import { qk } from '@/queries/keys'
import RunqLogo from '@/components/RunqLogo.vue'
import StatusBar from '@/components/StatusBar.vue'

const { t } = useI18n()
const route = useRoute()
const theme = useTheme()
const { mobile } = useDisplay()
const config = useConfigStore()
const settings = useSettingsStore()
const projects = useProjectStore()
const conn = useConnection()
const qc = useQueryClient()
// gpu chip in the app bar — caps-gated inside the query (enabled)
const { freeCount: gpuFree, totalCount: gpuTotal } = useGpuQuery()

const drawerOpen = ref(!mobile.value)
const collapsed = ref(localStorage.getItem('runq-sidebar-collapsed') === 'true')

// Temporary drawers must not cover the dashboard on first load or after an
// iPad orientation change. Permanent desktop navigation remains open.
watch(mobile, (isMobile) => {
  drawerOpen.value = !isMobile
})

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
  {
    name: 'overview',
    label: t('nav.overview'),
    icon: 'mdi-view-dashboard-outline',
    to: { name: 'overview' },
  },
  {
    name: 'submit',
    label: t('nav.submit'),
    icon: 'mdi-plus-circle-outline',
    to: { name: 'submit' },
  },
])

function isActive(name: string): boolean {
  return route.name === name
}

const breadcrumbs = computed(() => {
  const items: { title: string; to?: object; disabled?: boolean }[] = []
  if (route.params.project) {
    items.push({
      title: String(route.params.project),
      to: { name: 'project', params: { project: route.params.project } },
    })
  }
  if (route.params.jobId) {
    items.push({
      title: String(route.params.jobId).slice(0, 8),
      to: route.params.taskId
        ? {
            name: 'job-detail',
            params: { project: route.params.project, jobId: route.params.jobId },
          }
        : undefined,
      disabled: !route.params.taskId,
    })
  }
  if (route.params.taskId) {
    items.push({ title: String(route.params.taskId).slice(0, 8), disabled: true })
  }
  return items
})

const PROJECT_COLORS = [
  '#1E40AF',
  '#16A34A',
  '#D97706',
  '#DC2626',
  '#7C3AED',
  '#0891B2',
  '#DB2777',
  '#65A30D',
]
function projectColor(name: string): string {
  let hash = 0
  for (let i = 0; i < name.length; i++) hash = ((hash << 5) - hash + name.charCodeAt(i)) | 0
  return PROJECT_COLORS[Math.abs(hash) % PROJECT_COLORS.length]
}

function refreshShellData() {
  projects.fetch()
  // invalidating ['jobs'] refreshes every list variant wherever mounted
  qc.invalidateQueries({ queryKey: qk.jobs })
}

onMounted(async () => {
  try {
    if (!config.loaded) await config.fetchConfig()
    projects.fetch()
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
/* Nudge list-item prepend icons left to align with the nav visual grid. */
:deep(.v-list-item__prepend > .v-icon) {
  position: relative;
  left: -8px;
}
/* RQ-74: reconnect banner — amber, never red: the daemon being briefly away
   is a degraded state, not a failure verdict. */
.reconnect-banner {
  background: rgba(var(--v-theme-warning), 0.15);
  color: rgb(var(--v-theme-warning));
  border-bottom: 0.5px solid rgba(var(--v-theme-warning), 0.4);
}
</style>
