<template>
  <v-card class="pa-0">
    <div class="d-flex align-center ga-2 px-4 py-2 border-b">
      <v-icon size="16" color="primary">mdi-folder-outline</v-icon>
      <code class="text-caption font-mono text-truncate">{{ dataDir || '—' }}</code>
      <v-spacer />
      <v-btn
        v-if="dataDir" icon size="x-small" variant="text"
        :title="t('data.copy_path')" @click="copyRoot"
      >
        <v-icon size="14">mdi-content-copy</v-icon>
      </v-btn>
      <v-btn
        v-if="dataDir" icon size="x-small" variant="text"
        :title="t('common.refresh')" :disabled="loading" @click="load"
      >
        <v-icon size="14">mdi-refresh</v-icon>
      </v-btn>
    </div>

    <div v-if="!dataDir" class="pa-8 text-center text-body-2 text-on-surface-variant">
      {{ t('data.no_dir') }}
    </div>
    <div v-else-if="loading" class="d-flex justify-center pa-8">
      <v-progress-circular indeterminate color="primary" size="20" />
    </div>
    <div v-else-if="error" class="pa-6 text-caption text-error">{{ error }}</div>
    <div v-else-if="entries.length === 0" class="pa-8 text-center text-body-2 text-on-surface-variant">
      {{ t('data.empty_dir') }}
    </div>
    <div v-else class="py-1 data-tree">
      <DataTreeNode v-for="e in entries" :key="e.path" :entry="e" :target="target" :depth="0" />
    </div>

    <div class="px-4 py-2 text-caption text-on-surface-variant border-t">
      {{ t('data.note') }}
    </div>
  </v-card>
</template>

<script setup lang="ts">
// JobDataCard (RQ2-4 ②, kit ScreensC JobDataCard) — the job's data_dir on
// its OWNING target's filesystem (multi-target: the daemon's disk is not
// where an HPC job wrote). Root listing is fetched on tab entry;
// subdirectories load lazily per expand (DataTreeNode).
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { filesApi } from '@/apis/files'
import { useSnackbar } from '@/composables/useSnackbar'
import type { FSEntry } from '@/types/api'
import DataTreeNode from './DataTreeNode.vue'

const props = defineProps<{
  dataDir?: string
  target: string
}>()

const { t } = useI18n()
const snack = useSnackbar()

const loading = ref(false)
const error = ref('')
const entries = ref<FSEntry[]>([])

async function load() {
  if (!props.dataDir) return
  loading.value = true
  error.value = ''
  try {
    const list = await filesApi.list(props.dataDir, props.target)
    entries.value = [...list].sort((a, b) =>
      a.is_dir === b.is_dir ? a.name.localeCompare(b.name) : (a.is_dir ? -1 : 1))
  } catch (e: any) {
    error.value = e?.message || t('common.error')
  } finally {
    loading.value = false
  }
}
watch(() => props.dataDir, load, { immediate: true })

async function copyRoot() {
  if (!props.dataDir) return
  try {
    await navigator.clipboard.writeText(props.dataDir)
    snack.success(t('common.copied'))
  } catch {
    snack.error(t('common.error'))
  }
}
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.border-b { border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.border-t { border-top: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.data-tree { max-height: 480px; overflow-y: auto; }
</style>
