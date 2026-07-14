<template>
  <div>
    <div class="d-flex align-center justify-space-between mb-4">
      <h2 class="text-h6">{{ t('nav.log_viewer') }}</h2>
      <div class="d-flex ga-2">
        <v-btn size="small" variant="tonal" prepend-icon="mdi-content-paste" @click="pasteFromClipboard">
          {{ t('log.viewer_paste') }}
        </v-btn>
        <v-btn size="small" variant="tonal" prepend-icon="mdi-file-upload-outline" @click="fileInput?.click()">
          {{ t('log.viewer_open_file') }}
        </v-btn>
        <input ref="fileInput" type="file" accept=".log,.txt,.out,*" style="display:none" @change="onFileSelect" />
        <v-btn v-if="sessionId" size="small" variant="text" color="error" prepend-icon="mdi-close" @click="clear">
          {{ t('common.clear') }}
        </v-btn>
      </div>
    </div>

    <!-- Input area (when no content loaded) -->
    <v-card v-if="!sessionId" class="pa-0" variant="outlined">
      <v-textarea
        v-model="inputText"
        :placeholder="t('log.viewer_placeholder')"
        variant="plain"
        rows="12"
        auto-grow
        hide-details
        class="log-input"
      />
      <v-card-actions class="pa-3 pt-0">
        <v-spacer />
        <v-btn :disabled="!inputText.trim()" :loading="uploading" color="primary" variant="tonal" @click="loadInput">
          {{ t('log.viewer_process') }}
        </v-btn>
      </v-card-actions>
    </v-card>

    <!-- Processed log view -->
    <v-card v-else variant="outlined" class="log-viewer-card">
      <div class="d-flex align-center pa-3 pb-0 ga-2">
        <v-chip size="x-small" variant="tonal">{{ t('log.n_lines', { n: logLines.length }) }}</v-chip>
        <v-chip size="x-small" variant="tonal">{{ formatBytes(totalBytes) }}</v-chip>
        <v-btn
          v-if="endOffset < totalBytes"
          size="x-small" variant="text" :loading="loadingMore" @click="loadMore"
        >
          {{ t('log.load_more') }}
        </v-btn>
        <v-spacer />
      </div>
      <div class="d-flex flex-wrap">
        <!-- Log content (virtualized shared surface, incl. search bar) -->
        <!-- Uploaded sessions always page from byte 0, so the buffer base
             line number is 0 and absolute numbering stays valid. -->
        <LogSurfaceView
          :surface="surface"
          :items="renderItems"
          :log-loading="uploading"
          :line-number-base="0"
          :empty-text="t('log.no_output')"
          max-height="70vh"
        />
        <!-- Side panel -->
        <LogSidePanel
          :toggles="logStore.processors"
          :motif-groups="pipelineResult.motifGroups"
          :hidden-group-ids="effectiveHidden"
          :rules="logStore.preDrainRules"
          @toggle-processor="logStore.toggleProcessor"
          @toggle-cluster="toggleGroup"
          @scroll-to-group="scrollToGroup"
          @toggle-rule="logStore.toggleRule"
          @add-rule="logStore.addRule"
          @update-rule="logStore.updateRule"
          @remove-rule="logStore.removeRule"
        />
      </div>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { ref, nextTick, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useLogViewerStore } from '@/stores/logViewer'
import { utilsApi } from '@/apis/utils'
import LogSidePanel from '@/components/LogSidePanel.vue'
import LogSurfaceView from '@/components/LogSurfaceView.vue'
import { useLogSurface } from '@/composables/useLogSurface'

const { t } = useI18n()
const logStore = useLogViewerStore()

const inputText = ref('')
const fileInput = ref<HTMLInputElement>()
const logContainer = ref<HTMLElement>()

// ── Backend session state ──
const sessionId = ref('')
const logLines = ref<string[]>([])
const endOffset = ref(0)
const totalBytes = ref(0)
const uploading = ref(false)
const loadingMore = ref(false)

// ── Shared log surface (pipeline, fold state, search) — see useLogSurface.
// The whole object goes to LogSurfaceView; only the pieces the side panel
// and the Ctrl+F shortcut need are destructured here. ──
const surface = useLogSurface(logLines, logContainer)
const {
  pipelineResult, effectiveHidden, renderItems,
  toggleGroup, scrollToGroup, resetPipeline,
  searchOpen, searchInput,
} = surface

// ── Upload + read via backend ──
async function upload(content: string | Blob) {
  uploading.value = true
  try {
    const sess = await utilsApi.uploadLog(content)
    sessionId.value = sess.id
    totalBytes.value = sess.total_bytes
    resetPipeline()
    // Load all lines (up to 5000)
    await fetchPage(0, 5000, true)
  } catch { /* snackbar handled by api client */ }
  finally { uploading.value = false }
}

async function fetchPage(offset: number, lines: number, replace: boolean) {
  try {
    const page = await utilsApi.readLog(sessionId.value, { offset, lines })
    if (replace) {
      logLines.value = page.lines
    } else {
      logLines.value.push(...page.lines)
    }
    endOffset.value = page.next_offset
    totalBytes.value = page.size
  } catch { /* ignore */ }
}

async function loadMore() {
  if (endOffset.value >= totalBytes.value) return
  loadingMore.value = true
  try { await fetchPage(endOffset.value, 2000, false) }
  finally { loadingMore.value = false }
}

// ── Input handling ──
function loadInput() {
  if (inputText.value.trim()) upload(inputText.value)
}

async function pasteFromClipboard() {
  try {
    const text = await navigator.clipboard.readText()
    if (text.trim()) {
      inputText.value = ''
      upload(text)
    }
  } catch { /* clipboard access denied */ }
}

function onFileSelect(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  inputText.value = ''
  upload(file)
  if (fileInput.value) fileInput.value.value = ''
}

function clear() {
  if (sessionId.value) {
    utilsApi.deleteLog(sessionId.value).catch(() => {})
  }
  sessionId.value = ''
  logLines.value = []
  endOffset.value = 0
  totalBytes.value = 0
  inputText.value = ''
  resetPipeline()
}

function onKeydown(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
    if (sessionId.value) {
      e.preventDefault()
      searchOpen.value = true
      nextTick(() => searchInput.value?.focus())
    }
  }
}

onMounted(() => document.addEventListener('keydown', onKeydown))
onUnmounted(() => document.removeEventListener('keydown', onKeydown))

function formatBytes(b: number): string {
  if (b < 1024) return `${b} B`
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`
  if (b < 1024 * 1024 * 1024) return `${(b / (1024 * 1024)).toFixed(1)} MB`
  return `${(b / (1024 * 1024 * 1024)).toFixed(1)} GB`
}

// Cleanup session on unmount
onUnmounted(() => {
  if (sessionId.value) utilsApi.deleteLog(sessionId.value).catch(() => {})
})
</script>

<style scoped>
.log-input :deep(textarea) {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 0.8rem;
  line-height: 1.5;
  padding: 16px;
}
.log-viewer-card {
  overflow: hidden;
}
/* The log surface itself (lines, folds, search) lives in LogSurfaceView. */
</style>
