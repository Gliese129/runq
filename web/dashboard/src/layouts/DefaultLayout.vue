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
          <span class="d-flex">
            <v-btn
              icon
              size="x-small"
              variant="text"
              :aria-label="t('groups.new_group')"
              :title="t('groups.new_group')"
              @click="onCreateGroup"
            >
              <v-icon size="12">mdi-folder-plus-outline</v-icon>
            </v-btn>
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
          </span>
        </div>
        <v-tooltip v-else :text="t('overview.projects')" location="end">
          <template #activator="{ props: tp }">
            <div v-bind="tp" class="text-center mb-1">
              <v-icon size="16" color="on-surface-variant">mdi-folder-multiple-outline</v-icon>
            </div>
          </template>
        </v-tooltip>

        <!-- Grouped view (RQ2-4 ④, kit groups.jsx): a webUI-only lens —
             drag a project onto a group header to assign it. Collapsed
             rail keeps the flat list (groups need labels to mean much). -->
        <template v-if="!collapsed">
          <template v-for="g in groupedProjects.groups" :key="g.name">
            <div
              class="group-head d-flex align-center ga-1 px-2 rounded"
              :class="{ 'group-head--over': dragOverGroup === g.name }"
              role="button" tabindex="0"
              :aria-expanded="!g.collapsed"
              @click="groupsCtl.toggleCollapsed(g.name)"
              @keydown.enter="groupsCtl.toggleCollapsed(g.name)"
              @dragover.prevent="dragOverGroup = g.name"
              @dragleave="dragOverGroup = dragOverGroup === g.name ? '' : dragOverGroup"
              @drop.prevent="onDropOnGroup(g.name)"
            >
              <v-icon size="13">{{ g.collapsed ? 'mdi-chevron-right' : 'mdi-chevron-down' }}</v-icon>
              <template v-if="renamingGroup === g.name">
                <input
                  ref="renameInput"
                  v-model="renameValue"
                  class="rename-input"
                  @click.stop
                  @keydown.enter="commitRename(g.name)"
                  @keydown.esc="renamingGroup = ''"
                  @blur="commitRename(g.name)"
                >
              </template>
              <span v-else class="text-caption font-weight-medium flex-grow-1 text-truncate">{{ g.name }}</span>
              <!-- Ruling: the badge is job_count only. -->
              <span class="text-caption text-on-surface-variant">{{ g.jobCount }}</span>
              <v-menu location="bottom end">
                <template #activator="{ props: menuProps }">
                  <v-btn
                    icon size="x-small" variant="text" density="comfortable"
                    :aria-label="t('groups.group_menu')"
                    v-bind="menuProps" @click.stop
                  >
                    <v-icon size="12">mdi-dots-horizontal</v-icon>
                  </v-btn>
                </template>
                <v-list density="compact">
                  <v-list-item @click="startRename(g.name)">
                    <v-list-item-title class="text-body-2">{{ t('submit.rename') }}</v-list-item-title>
                  </v-list-item>
                  <v-list-item @click="groupsCtl.remove(g.name)">
                    <v-list-item-title class="text-body-2">{{ t('groups.dissolve') }}</v-list-item-title>
                  </v-list-item>
                </v-list>
              </v-menu>
            </div>
            <template v-if="!g.collapsed">
              <v-list-item
                v-for="proj in g.members"
                :key="proj.name"
                :to="{ name: 'project', params: { project: proj.name } }"
                :active="projects.selected === proj.name"
                :aria-label="proj.name"
                density="compact"
                rounded="lg"
                class="mb-1 ml-3 project-row"
                color="primary"
                draggable="true"
                @dragstart="onDragStart(proj.name, $event)"
                @click="projects.select(proj.name)"
              >
                <template #prepend>
                  <div class="status-dot mr-2" :style="{ background: projectColor(proj.name) }" />
                </template>
                <v-list-item-title class="text-body-2">{{ proj.name }}</v-list-item-title>
                <template #append>
                  <span class="text-caption text-on-surface-variant">{{ proj.job_count }}</span>
                </template>
              </v-list-item>
            </template>
          </template>

          <!-- Ungrouped tail — also the drop zone for "take it out". -->
          <div
            v-if="groupedProjects.groups.length > 0 && groupedProjects.ungrouped.length > 0"
            class="text-caption text-on-surface-variant px-2 mt-1"
            :class="{ 'group-head--over rounded': dragOverGroup === UNGROUPED_ZONE }"
            @dragover.prevent="dragOverGroup = UNGROUPED_ZONE"
            @dragleave="dragOverGroup = dragOverGroup === UNGROUPED_ZONE ? '' : dragOverGroup"
            @drop.prevent="onDropOnGroup('')"
          >{{ t('groups.ungrouped') }}</div>
          <v-list-item
            v-for="proj in groupedProjects.ungrouped"
            :key="proj.name"
            :to="{ name: 'project', params: { project: proj.name } }"
            :active="projects.selected === proj.name"
            :aria-label="proj.name"
            density="compact"
            rounded="lg"
            class="mb-1 project-row"
            color="primary"
            draggable="true"
            @dragstart="onDragStart(proj.name, $event)"
            @click="projects.select(proj.name)"
          >
            <template #prepend>
              <div class="status-dot mr-2" :style="{ background: projectColor(proj.name) }" />
            </template>
            <v-list-item-title class="text-body-2">{{ proj.name }}</v-list-item-title>
            <template #append>
              <span class="text-caption text-on-surface-variant">{{ proj.job_count }}</span>
            </template>
          </v-list-item>
        </template>

        <!-- Collapsed rail: flat dots, no group chrome. -->
        <template v-else>
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
          </v-list-item>
        </template>

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
          <!-- Same control shape as Dark mode / Collapse below — a
               v-list-item here had its own prepend spacer and icon nudge,
               so the footer trio never lined up. -->
          <v-btn
            block
            variant="text"
            density="comfortable"
            rounded="lg"
            class="nav-btn mb-1"
            :class="collapsed ? 'justify-center' : 'justify-start px-3'"
            prepend-icon="mdi-cog-outline"
            :color="isActive('settings') ? 'primary' : undefined"
            :aria-label="t('nav.settings')"
            :to="{ name: 'settings' }"
          >
            <span v-if="!collapsed" class="text-body-2">{{ t('nav.settings') }}</span>
          </v-btn>
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
import { useProjectGroups } from '@/composables/useProjectGroups'
import { groupProjects } from '@/composables/projectGroups'
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

// ── Project groups (RQ2-4 ④): webUI-only lens over the flat list.
// Seed from work_dir parents once projects load and nothing was stored. ──
const groupsCtl = useProjectGroups()
const UNGROUPED_ZONE = '__ungrouped__' // drag-over marker, never a group name
const dragOverGroup = ref('')
const renamingGroup = ref('')
const renameValue = ref('')
const renameInput = ref<HTMLInputElement[]>()

watch(() => projects.visible, (list) => {
  if (list.length > 0) groupsCtl.seedIfEmpty(list)
}, { immediate: true })

const groupedProjects = computed(() => groupProjects(projects.visible, groupsCtl.state.value))

// dataTransfer.getData is unreadable during dragover on some engines —
// track the dragged project ourselves; drop reads this, not the transfer.
const lastDragged = ref('')

function onDragStart(project: string, e: DragEvent) {
  lastDragged.value = project
  e.dataTransfer?.setData('text/plain', project)
  if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move'
}
function onDropOnGroup(group: string) {
  dragOverGroup.value = ''
  if (lastDragged.value) void groupsCtl.assign(lastDragged.value, group)
  lastDragged.value = ''
}

async function onCreateGroup() {
  await groupsCtl.create()
}

function startRename(group: string) {
  renamingGroup.value = group
  renameValue.value = group
  requestAnimationFrame(() => renameInput.value?.[0]?.focus())
}
function commitRename(from: string) {
  const to = renameValue.value
  renamingGroup.value = ''
  if (to.trim() && to !== from) void groupsCtl.rename(from, to)
}

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
  {
    name: 'target',
    label: t('nav.targets'),
    icon: 'mdi-server-outline',
    to: { name: 'target' },
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
  void groupsCtl.load() // roaming ui.json copy — local cache already applied
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
/* Project group rows (RQ2-4 ④) */
.group-head {
  min-height: 26px;
  cursor: pointer;
  user-select: none;
}
.group-head:hover { background: rgb(var(--v-theme-surface-variant), 0.4); }
.group-head--over {
  background: rgb(var(--v-theme-primary), 0.12);
  outline: 1px dashed rgb(var(--v-theme-primary), 0.5);
}
.rename-input {
  flex: 1;
  min-width: 0;
  font-size: 12px;
  font-family: inherit;
  border: 1px solid rgb(var(--v-theme-primary));
  border-radius: 4px;
  padding: 1px 4px;
  background: rgb(var(--v-theme-surface));
  color: rgb(var(--v-theme-on-surface));
  outline: none;
}

/* RQ-74: reconnect banner — amber, never red: the daemon being briefly away
   is a degraded state, not a failure verdict. */
.reconnect-banner {
  background: rgba(var(--v-theme-warning), 0.15);
  color: rgb(var(--v-theme-warning));
  border-bottom: 0.5px solid rgba(var(--v-theme-warning), 0.4);
}
</style>
