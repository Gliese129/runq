<template>
  <div>
    <div
      class="tree-row d-flex align-center ga-1 px-2"
      :style="{ paddingLeft: `${8 + depth * 16}px` }"
      :role="entry.is_dir ? 'button' : undefined"
      :tabindex="entry.is_dir ? 0 : undefined"
      :aria-expanded="entry.is_dir ? open : undefined"
      @click="toggle"
      @keydown.enter="toggle"
    >
      <v-icon size="14" :color="entry.is_dir ? 'primary' : undefined">
        {{ entry.is_dir ? (open ? 'mdi-folder-open-outline' : 'mdi-folder-outline') : 'mdi-file-outline' }}
      </v-icon>
      <span class="text-body-2 font-mono">{{ entry.name }}</span>
      <v-progress-circular v-if="loading" indeterminate size="10" width="2" color="primary" />
      <v-spacer />
      <span v-if="!entry.is_dir" class="text-caption text-on-surface-variant font-mono">
        {{ fmtSize(entry.size) }}
      </span>
      <v-btn
        icon size="x-small" variant="text" density="comfortable"
        :title="t('data.copy_path')"
        @click.stop="copyPath"
      >
        <v-icon size="12">mdi-content-copy</v-icon>
      </v-btn>
    </div>
    <div v-if="loadError" class="text-caption text-error" :style="{ paddingLeft: `${28 + depth * 16}px` }">
      {{ loadError }}
    </div>
    <template v-if="open && children">
      <div v-if="children.length === 0" class="text-caption text-on-surface-variant py-1" :style="{ paddingLeft: `${28 + depth * 16}px` }">
        {{ t('data.empty_dir') }}
      </div>
      <DataTreeNode
        v-for="c in children" :key="c.path"
        :entry="c" :target="target" :depth="depth + 1"
      />
    </template>
  </div>
</template>

<script setup lang="ts">
// DataTreeNode (RQ2-4 ②, kit ScreensC FileNode) — one row of the data_dir
// tree. Directories fetch their children on FIRST expand only (task dirs
// hold checkpoints; an eager walk would hammer the owning target's fs).
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { filesApi } from '@/apis/files'
import { useSnackbar } from '@/composables/useSnackbar'
import type { FSEntry } from '@/types/api'

const props = defineProps<{
  entry: FSEntry
  /** Owning target — data_dir lives on job.target's filesystem. */
  target: string
  depth: number
}>()

const { t } = useI18n()
const snack = useSnackbar()

const open = ref(false)
const loading = ref(false)
const loadError = ref('')
const children = ref<FSEntry[] | null>(null)

async function toggle() {
  if (!props.entry.is_dir) return
  open.value = !open.value
  if (open.value && children.value === null && !loading.value) {
    loading.value = true
    loadError.value = ''
    try {
      const list = await filesApi.list(props.entry.path, props.target)
      children.value = [...list].sort((a, b) =>
        a.is_dir === b.is_dir ? a.name.localeCompare(b.name) : (a.is_dir ? -1 : 1))
    } catch (e: any) {
      loadError.value = e?.message || t('common.error')
      open.value = false
      // children stays null — the next expand retries
    } finally {
      loading.value = false
    }
  }
}

async function copyPath() {
  try {
    await navigator.clipboard.writeText(props.entry.path)
    snack.success(t('common.copied'))
  } catch {
    snack.error(t('common.error'))
  }
}

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.tree-row {
  min-height: 28px;
  border-radius: 4px;
}
.tree-row[role='button'] { cursor: pointer; }
.tree-row:hover { background: rgb(var(--v-theme-surface-variant), 0.4); }
</style>
