<template>
  <v-row no-gutters style="max-width: 1060px; margin: 0 auto">
    <!-- Left: Quick access (pinned + recent + from-job) -->
    <v-col cols="12" md="3" class="pr-md-4">
      <!-- Pinned workspaces -->
      <div v-if="preferredWorkspaces.length > 0" class="mb-4">
        <div class="text-caption text-on-surface-variant mb-2 d-flex align-center ga-1">
          <v-icon size="12">mdi-star</v-icon> {{ t('submit.workspaces') }}
        </div>
        <div
          v-for="ws in preferredWorkspaces"
          :key="ws"
          class="quick-item d-flex align-center ga-2 pa-2 rounded cursor-pointer"
          :class="{ 'bg-primary': currentPath === ws }"
          @click="navigateTo(ws)"
        >
          <v-icon size="14" :color="currentPath === ws ? 'on-primary' : 'primary'">mdi-folder-star-outline</v-icon>
          <span class="text-body-2 text-truncate" :class="currentPath === ws ? 'text-on-primary' : ''">
            {{ ws.split('/').filter(Boolean).pop() || ws }}
          </span>
          <v-spacer />
          <v-btn
            icon size="x-small" variant="text"
            @click.stop="prefs.removePreferredWorkspace(ws)"
          >
            <v-icon size="12" :color="currentPath === ws ? 'on-primary' : 'on-surface-variant'">mdi-close</v-icon>
          </v-btn>
        </div>
      </div>

      <!-- Recent scripts -->
      <div v-if="recentScripts.length > 0" class="mb-4">
        <div class="text-caption text-on-surface-variant mb-2 d-flex align-center ga-1">
          <v-icon size="12">mdi-clock-outline</v-icon> {{ t('submit.recent') }}
        </div>
        <div
          v-for="s in recentScripts"
          :key="s.path"
          class="quick-item d-flex align-center ga-2 pa-2 rounded cursor-pointer"
          @click="quickSelect(s.path, s.name)"
        >
          <v-icon size="14" color="primary">mdi-file-code-outline</v-icon>
          <div class="flex-grow-1" style="min-width: 0">
            <div class="text-body-2 font-weight-medium text-truncate">{{ s.name }}</div>
            <div class="text-caption text-on-surface-variant">{{ s.project }} · {{ timeAgo(s.ts) }}</div>
          </div>
        </div>
      </div>

      <!-- From previous job -->
      <div v-if="state.fromJobId">
        <div class="text-caption text-on-surface-variant mb-2 d-flex align-center ga-1">
          <v-icon size="12">mdi-content-copy</v-icon> {{ t('submit.from_job') }}
        </div>
        <div
          class="quick-item d-flex align-center ga-2 pa-2 rounded cursor-pointer"
          @click="$emit('loadFromJob')"
        >
          <code class="text-caption text-primary">{{ state.fromJobId.slice(0, 8) }}</code>
          <v-spacer />
          <v-icon size="14" color="primary">mdi-arrow-right</v-icon>
        </div>
      </div>
    </v-col>

    <!-- Right: Path input + File browser -->
    <v-col cols="12" md="9">
      <v-card class="pa-4">
        <!-- Path input -->
        <v-text-field
          v-model="pastedPath"
          :placeholder="t('submit.paste_path')"
          prepend-inner-icon="mdi-link-variant"
          density="compact"
          variant="outlined"
          hide-details
          clearable
          class="mb-3"
          style="font-family: monospace"
          @keydown.enter="usePastedPath"
          @update:model-value="onPathInput"
        />

        <!-- Toolbar: breadcrumb + filters -->
        <div class="d-flex align-center justify-space-between mb-2">
          <!-- Breadcrumb -->
          <div class="d-flex align-center flex-wrap ga-1 text-caption text-on-surface-variant" style="min-width: 0">
            <span class="cursor-pointer breadcrumb-seg" @click="loadDir('', true)">~</span>
            <template v-for="(seg, i) in pathSegments" :key="i">
              <v-icon size="10">mdi-chevron-right</v-icon>
              <span
                class="cursor-pointer breadcrumb-seg"
                :class="{ 'font-weight-medium': i === pathSegments.length - 1 }"
                @click="navigateToSegment(i)"
              >{{ seg }}</span>
            </template>
            <!-- Pin button -->
            <v-btn
              v-if="currentPath"
              icon size="x-small" variant="text"
              :color="isWorkspacePinned ? 'primary' : undefined"
              @click="togglePinWorkspace"
              class="ml-1"
            >
              <v-icon size="14">{{ isWorkspacePinned ? 'mdi-star' : 'mdi-star-outline' }}</v-icon>
            </v-btn>
          </div>

          <!-- Filters -->
          <div class="d-flex align-center ga-2 flex-shrink-0">
            <v-btn
              icon size="x-small"
              :variant="showHidden ? 'tonal' : 'text'"
              :color="showHidden ? 'primary' : undefined"
              @click="showHidden = !showHidden"
            >
              <v-icon size="14">mdi-eye{{ showHidden ? '' : '-off' }}-outline</v-icon>
            </v-btn>
            <v-btn-toggle v-model="fileFilter" mandatory density="compact" variant="outlined" rounded="0">
              <v-btn value=".py" size="x-small">.py</v-btn>
              <v-btn value=".yaml" size="x-small">.yaml</v-btn>
              <v-btn value="" size="x-small">{{ t('submit.all_files') }}</v-btn>
            </v-btn-toggle>
          </div>
        </div>

        <!-- File list -->
        <v-list density="compact" class="rounded pa-0" style="max-height: 400px; overflow-y: auto">
          <v-list-item
            v-if="currentPath !== ''"
            @click="navigateUp"
            class="rounded"
          >
            <template #prepend>
              <v-icon size="16" color="on-surface-variant">mdi-arrow-up</v-icon>
            </template>
            <v-list-item-title class="text-body-2 text-on-surface-variant">..</v-list-item-title>
          </v-list-item>

          <v-list-item
            v-for="entry in filteredEntries"
            :key="entry.path"
            @click="entry.is_dir ? navigateTo(entry.path) : selectAndProceed(entry)"
            class="rounded"
            :active="state.selectedScript?.path === entry.path"
          >
            <template #prepend>
              <v-icon size="16" :color="entry.is_dir ? 'warning' : 'primary'">
                {{ entry.is_dir ? 'mdi-folder' : 'mdi-file-code-outline' }}
              </v-icon>
            </template>
            <v-list-item-title class="text-body-2">{{ entry.name }}</v-list-item-title>
            <template #append>
              <span v-if="!entry.is_dir" class="text-caption text-on-surface-variant">
                {{ formatSize(entry.size) }}
              </span>
            </template>
          </v-list-item>
        </v-list>

        <div v-if="filteredEntries.length === 0 && !loadingDir" class="text-center text-on-surface-variant pa-8">
          <v-icon size="28" class="mb-2" color="on-surface-variant">mdi-file-search-outline</v-icon>
          <div class="text-body-2">{{ t('submit.no_files') }}</div>
        </div>
      </v-card>
    </v-col>
  </v-row>
</template>

<script setup lang="ts">
import { ref, computed, inject, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { api } from '@/apis/client'
import { usePreferences } from '@/composables/usePreferences'
import type { FSEntry } from '@/types/api'
import { SUBMIT_STATE_KEY } from '@/types/submit'

const emit = defineEmits<{
  select: [entry: FSEntry]
  loadFromJob: []
}>()

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const prefs = usePreferences()

const preferredWorkspaces = computed(() => prefs.preferredWorkspaces.value)
const recentScripts = computed(() => prefs.recentScripts.value)

const pastedPath = ref('')
const currentPath = ref('')
const entries = ref<FSEntry[]>([])
const loadingDir = ref(false)
const fileFilter = ref('.py')
const showHidden = ref(false)

const filteredEntries = computed(() => {
  let list = entries.value
  if (!showHidden.value) {
    list = list.filter(e => !e.name.startsWith('.'))
  }
  if (fileFilter.value) {
    list = list.filter(e => e.is_dir || e.name.endsWith(fileFilter.value))
  }
  return list
})

// --- Path input debounce ---

const SCRIPT_EXTS = ['.py', '.yaml', '.yml', '.sh', '.r', '.jl']
let debounceTimer: ReturnType<typeof setTimeout> | null = null

function onPathInput(value: string | null) {
  if (debounceTimer) clearTimeout(debounceTimer)
  if (!value) return
  debounceTimer = setTimeout(() => {
    const trimmed = value.trim()
    if (SCRIPT_EXTS.some(ext => trimmed.toLowerCase().endsWith(ext))) {
      usePastedPath()
      return
    }
    if (trimmed.endsWith('/')) {
      loadDir(trimmed.replace(/\/+$/, ''), false)
      return
    }
    const lastSlash = trimmed.lastIndexOf('/')
    if (lastSlash > 0) {
      const dir = trimmed.substring(0, lastSlash)
      if (dir !== currentPath.value) loadDir(dir, false)
    }
  }, 300)
}

// --- Workspace helpers ---

const isWorkspacePinned = computed(() =>
  currentPath.value !== '' && prefs.preferredWorkspaces.value.includes(currentPath.value)
)

function togglePinWorkspace() {
  if (!currentPath.value) return
  if (isWorkspacePinned.value) prefs.removePreferredWorkspace(currentPath.value)
  else prefs.addPreferredWorkspace(currentPath.value)
}

// --- Directory navigation ---

async function loadDir(path: string, syncInput = true) {
  loadingDir.value = true
  try {
    currentPath.value = path
    if (syncInput) pastedPath.value = path ? path + '/' : ''
    entries.value = await api.get<FSEntry[]>(`/fs/list?path=${encodeURIComponent(path)}`)
  } catch { /* toast handled by api client */ }
  finally { loadingDir.value = false }
}

function navigateTo(path: string) { loadDir(path, true) }

const pathSegments = computed(() => {
  if (!currentPath.value) return []
  return currentPath.value.split('/').filter(Boolean)
})

function navigateToSegment(index: number) {
  const segs = pathSegments.value.slice(0, index + 1)
  loadDir('/' + segs.join('/'), true)
}

function navigateUp() {
  const parts = currentPath.value.split('/').filter(Boolean)
  parts.pop()
  loadDir(parts.length > 0 ? '/' + parts.join('/') : '')
}

// --- Script selection ---

function selectAndProceed(entry: FSEntry) { emit('select', entry) }

function quickSelect(path: string, name: string) {
  emit('select', { path, name, is_dir: false, size: 0 })
}

function usePastedPath() {
  if (!pastedPath.value.trim()) return
  const path = pastedPath.value.trim()
  const name = path.split('/').pop() || path
  emit('select', { path, name, is_dir: false, size: 0 })
}

// --- Helpers ---

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1048576) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1048576).toFixed(1)} MB`
}

function timeAgo(ts: number): string {
  const diff = (Date.now() - ts) / 1000
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

// --- Init ---

onMounted(() => {
  const defaultDir = prefs.preferredWorkspaces.value[0] || ''
  loadDir(defaultDir)
})
</script>

<style scoped>
.quick-item {
  transition: background 0.15s ease;
}
.quick-item:hover {
  background: rgb(var(--v-theme-surface-variant));
}
.breadcrumb-seg {
  text-decoration: underline dotted;
  text-underline-offset: 2px;
}
.breadcrumb-seg:hover {
  color: rgb(var(--v-theme-primary));
}
</style>
/style>
