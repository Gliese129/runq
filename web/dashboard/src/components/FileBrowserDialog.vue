<template>
  <v-dialog v-model="open" max-width="720">
    <v-card class="pa-4">
      <div class="d-flex align-center justify-space-between mb-3">
        <div class="text-subtitle-1 font-weight-medium">{{ title }}</div>
        <div class="d-flex ga-1">
          <v-btn
            v-if="mode === 'directory'"
            size="small" variant="tonal" color="primary"
            :disabled="!currentDir"
            @click="emitSelect(currentDir)"
          >
            <v-icon size="14" start>mdi-check</v-icon> Use this folder
          </v-btn>
          <v-btn icon size="small" variant="text" @click="open = false">
            <v-icon>mdi-close</v-icon>
          </v-btn>
        </div>
      </div>

      <v-row no-gutters>
        <!-- Sidebar -->
        <v-col cols="4" class="pr-3" style="max-height: 460px; overflow-y: auto">
          <div v-if="prefs.preferredWorkspaces.value.length > 0" class="mb-3">
            <div class="text-caption text-on-surface-variant mb-1 d-flex align-center ga-1">
              <v-icon size="11">mdi-star</v-icon> Favorites
            </div>
            <div
              v-for="ws in prefs.preferredWorkspaces.value" :key="ws"
              class="quick-item d-flex align-center ga-2 pa-2 rounded cursor-pointer"
              :class="{ 'bg-primary': currentDir === ws }"
              @click="loadDir(ws)"
            >
              <v-icon size="14" :color="currentDir === ws ? 'on-primary' : 'primary'">mdi-folder-star-outline</v-icon>
              <span class="text-body-2 text-truncate" :class="currentDir === ws ? 'text-on-primary' : ''">
                {{ ws.split('/').filter(Boolean).pop() || ws }}
              </span>
            </div>
          </div>

          <div v-if="mode === 'script' && prefs.recentScripts.value.length > 0">
            <div class="text-caption text-on-surface-variant mb-1 d-flex align-center ga-1">
              <v-icon size="11">mdi-clock-outline</v-icon> Recent
            </div>
            <div
              v-for="s in prefs.recentScripts.value" :key="s.path"
              class="quick-item d-flex align-center ga-2 pa-2 rounded cursor-pointer"
              @click="emitSelect(s.path)"
            >
              <v-icon size="14" color="primary">mdi-file-code-outline</v-icon>
              <div class="flex-grow-1" style="min-width: 0">
                <div class="text-body-2 font-weight-medium text-truncate">{{ s.name }}</div>
                <div class="text-caption text-on-surface-variant">{{ s.project }}</div>
              </div>
            </div>
          </div>

          <div v-if="prefs.preferredWorkspaces.value.length === 0 && !(mode === 'script' && prefs.recentScripts.value.length > 0)"
            class="text-caption text-on-surface-variant pa-2"
          >
            No favorites yet
          </div>
        </v-col>

        <!-- File list -->
        <v-col cols="8">
          <v-text-field
            v-model="pathInput"
            placeholder="Paste path or browse..."
            prepend-inner-icon="mdi-link-variant"
            density="compact" variant="outlined" hide-details clearable
            class="mb-2" style="font-family: monospace; font-size: 12px"
            @keydown.enter="usePathInput"
          />

          <!-- Breadcrumb -->
          <div class="d-flex align-center flex-wrap ga-1 text-caption text-on-surface-variant mb-2">
            <span class="cursor-pointer breadcrumb-seg" @click="loadDir('')">~</span>
            <template v-for="(seg, i) in segments" :key="i">
              <v-icon size="10">mdi-chevron-right</v-icon>
              <span
                class="cursor-pointer breadcrumb-seg"
                :class="{ 'font-weight-medium': i === segments.length - 1 }"
                @click="loadDir('/' + segments.slice(0, i + 1).join('/'))"
              >{{ seg }}</span>
            </template>
            <v-btn v-if="currentDir" icon size="x-small" variant="text" :color="isPinned ? 'primary' : undefined" @click="togglePin" class="ml-1">
              <v-icon size="14">{{ isPinned ? 'mdi-star' : 'mdi-star-outline' }}</v-icon>
            </v-btn>
          </div>

          <v-list density="compact" class="rounded pa-0" style="max-height: 340px; overflow-y: auto">
            <v-list-item v-if="currentDir" @click="navigateUp" class="rounded">
              <template #prepend><v-icon size="16" color="on-surface-variant">mdi-arrow-up</v-icon></template>
              <v-list-item-title class="text-body-2 text-on-surface-variant">..</v-list-item-title>
            </v-list-item>
            <v-list-item
              v-for="entry in filteredEntries" :key="entry.path"
              @click="onEntryClick(entry)"
              class="rounded"
            >
              <template #prepend>
                <v-icon size="16" :color="entry.is_dir ? 'warning' : 'primary'">
                  {{ entry.is_dir ? 'mdi-folder' : 'mdi-file-code-outline' }}
                </v-icon>
              </template>
              <v-list-item-title class="text-body-2">{{ entry.name }}</v-list-item-title>
            </v-list-item>
          </v-list>
        </v-col>
      </v-row>
    </v-card>
  </v-dialog>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { filesApi } from '@/apis/files'
import { usePreferences } from '@/composables/usePreferences'
import type { FSEntry } from '@/types/api'

export type BrowseMode = 'script' | 'directory' | 'file' | 'folder'

const props = withDefaults(defineProps<{
  modelValue: boolean
  mode?: BrowseMode
  initialDir?: string
  fileFilter?: string  // e.g. '.py' — only show files matching this extension
}>(), {
  mode: 'script',
  initialDir: '',
  fileFilter: '.py,.sh',
})

const emit = defineEmits<{
  'update:modelValue': [v: boolean]
  'select': [path: string]
}>()

const prefs = usePreferences()

const open = computed({
  get: () => props.modelValue,
  set: v => emit('update:modelValue', v),
})

const currentDir = ref('')
const entries = ref<FSEntry[]>([])
const pathInput = ref('')

const segments = computed(() => currentDir.value ? currentDir.value.split('/').filter(Boolean) : [])

const isPinned = computed(() => currentDir.value !== '' && prefs.preferredWorkspaces.value.includes(currentDir.value))

const title = computed(() => {
  switch (props.mode) {
    case 'script': return 'Select Script'
    case 'directory': return 'Select Working Directory'
    case 'file': return 'Select File'
    case 'folder': return 'Select Folder'
  }
})

const filteredEntries = computed(() =>
  entries.value.filter(e => {
    if (e.name.startsWith('.')) return false
    if (props.mode === 'directory' || props.mode === 'folder') return e.is_dir
    if (e.is_dir) return true
    if (props.fileFilter) {
      // comma-separated extension list, e.g. ".py,.sh,.yaml,.yml"
      return props.fileFilter.split(',').some(ext => e.name.endsWith(ext.trim()))
    }
    return true
  })
)

watch(() => props.modelValue, (v) => {
  if (v) loadDir(props.initialDir || prefs.preferredWorkspaces.value[0] || '')
})

async function loadDir(path: string) {
  currentDir.value = path
  pathInput.value = path ? path + '/' : ''
  try {
    entries.value = await filesApi.list(path)
  } catch { entries.value = [] }
}

function navigateUp() {
  const parts = currentDir.value.split('/').filter(Boolean)
  parts.pop()
  loadDir(parts.length > 0 ? '/' + parts.join('/') : '')
}

function togglePin() {
  if (!currentDir.value) return
  if (isPinned.value) prefs.removePreferredWorkspace(currentDir.value)
  else prefs.addPreferredWorkspace(currentDir.value)
}

function onEntryClick(entry: FSEntry) {
  if (entry.is_dir) {
    if (props.mode === 'folder') {
      emitSelect(entry.path)
    } else {
      loadDir(entry.path)
    }
  } else {
    emitSelect(entry.path)
  }
}

function usePathInput() {
  const p = pathInput.value.trim()
  if (!p) return
  // If it looks like a file path, select it directly
  if (props.mode === 'script' && p.endsWith('.py')) {
    emitSelect(p)
  } else if (props.mode === 'file' && !p.endsWith('/')) {
    emitSelect(p)
  } else {
    loadDir(p.replace(/\/+$/, ''))
  }
}

function emitSelect(path: string) {
  emit('select', path)
  open.value = false
}
</script>

<style scoped>
.quick-item { transition: background 0.15s ease; }
.quick-item:hover { background: rgb(var(--v-theme-surface-variant)); }
.breadcrumb-seg { text-decoration: underline dotted; text-underline-offset: 2px; }
.breadcrumb-seg:hover { color: rgb(var(--v-theme-primary)); }
</style>
