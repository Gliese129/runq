<template>
  <!-- RQ-74: runq self-logs — when a death never reaches the UI push path
       (daemon panic, sensor failure, forward churn), its cause is in
       daemon.log. This panel means "read it without leaving the browser". -->
  <v-card class="mb-4 pa-5">
    <div class="d-flex align-center ga-2 mb-1">
      <div class="text-subtitle-2">{{ t('settings.runq_logs') }}</div>
      <v-spacer />
      <v-btn size="x-small" variant="text" :loading="loading" @click="reload">
        <v-icon start size="12">mdi-refresh</v-icon>
        {{ t('common.refresh') }}
      </v-btn>
      <v-switch
        v-model="following"
        density="compact"
        hide-details
        inline
        color="primary"
        :label="t('log.follow')"
      />
    </div>
    <div class="text-caption text-on-surface-variant mb-3">
      {{ t('settings.runq_logs_hint') }}
    </div>

    <div v-if="files.length === 0" class="text-caption text-on-surface-variant">
      {{ t('settings.no_daemon_logs') }}
    </div>

    <template v-else>
      <div class="d-flex flex-wrap align-center ga-2 mb-3">
        <v-chip-group v-model="selected" mandatory selected-class="text-primary">
          <v-chip v-for="f in files" :key="f.name" :value="f.name" size="small" variant="tonal">
            {{ f.name }}.log
            <span class="text-caption text-on-surface-variant ml-1">{{ formatBytes(f.size) }}</span>
          </v-chip>
        </v-chip-group>
        <v-spacer />
        <v-select
          v-model="levelFilter"
          :items="levelOptions"
          density="compact"
          hide-details
          variant="outlined"
          style="max-width: 130px"
          :aria-label="t('settings.log_level')"
        />
        <v-text-field
          v-model="keyword"
          density="compact"
          hide-details
          variant="outlined"
          clearable
          prepend-inner-icon="mdi-magnify"
          :placeholder="t('settings.log_keyword')"
          style="max-width: 220px"
        />
      </div>

      <div ref="logContainer">
        <LogSurfaceView
          :surface="surface"
          :items="surface.renderItems.value"
          :log-loading="loading"
          :line-number-base="-1"
          :empty-text="t('log.no_output')"
          max-height="50vh"
        />
      </div>
    </template>
  </v-card>
</template>

<script setup lang="ts">
import { ref, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { daemonLogsApi, type DaemonLogFile } from '@/apis/daemonLogs'
import LogSurfaceView from '@/components/LogSurfaceView.vue'
import { useLogSurface } from '@/composables/useLogSurface'

const { t } = useI18n()

const files = ref<DaemonLogFile[]>([])
const selected = ref('daemon')
const loading = ref(false)
const following = ref(false)

// Raw buffer + view buffer: filters run OVER the fetched bytes (client-side
// — these files are local to the daemon and one page is bounded), so
// changing a filter never refetches.
const rawLines = ref<string[]>([])
const endOffset = ref(0)
const MAX_BUFFER_LINES = 5000

const levelFilter = ref('all')
const levelOptions = ['all', 'error', 'warn', 'info', 'debug']
const keyword = ref('')

const viewLines = ref<string[]>([])
const logContainer = ref<HTMLElement>()
const surface = useLogSurface(viewLines, logContainer)

function applyFilters() {
  const lvl = levelFilter.value
  const kw = (keyword.value ?? '').toLowerCase()
  viewLines.value = rawLines.value.filter((line) => {
    const l = line.toLowerCase()
    if (lvl !== 'all' && !l.includes(`level=${lvl}`)) return false
    if (kw && !l.includes(kw)) return false
    return true
  })
}
watch([rawLines, levelFilter, keyword], applyFilters, { deep: false })

async function reload() {
  loading.value = true
  try {
    const res = await daemonLogsApi.list()
    // Defensive on purpose (RQ-74 review finding 3): an older daemon (404
    // handler variants), a proxy, or a test mock may answer with a shape
    // that lacks `files`. A debug aid must degrade to "no logs", never
    // crash the Settings render.
    files.value = Array.isArray(res?.files) ? res.files : []
    if (files.value.length === 0) return
    if (!files.value.some((f) => f.name === selected.value)) {
      selected.value = files.value[0].name
    }
    const page = await daemonLogsApi.page(selected.value, { tail: true })
    rawLines.value = Array.isArray(page?.lines) ? page.lines : []
    endOffset.value = page?.next_offset ?? 0
  } catch {
    /* silent — the panel is a debug aid, not a critical path */
  } finally {
    loading.value = false
  }
}

// Follow: cheap 2s polling from the last offset (append-only; a rotation —
// size below our offset — resets to the tail).
let timer: ReturnType<typeof setInterval> | null = null
async function poll() {
  try {
    const page = await daemonLogsApi.page(selected.value, { offset: endOffset.value })
    if (!page || !Array.isArray(page.lines)) return // malformed response: skip this tick
    if (page.rotated || page.size < endOffset.value) {
      const tail = await daemonLogsApi.page(selected.value, { tail: true })
      rawLines.value = Array.isArray(tail?.lines) ? tail.lines : []
      endOffset.value = tail?.next_offset ?? 0
      return
    }
    if (page.lines.length > 0) {
      const merged = rawLines.value.concat(page.lines)
      rawLines.value =
        merged.length > MAX_BUFFER_LINES ? merged.slice(merged.length - MAX_BUFFER_LINES) : merged
      endOffset.value = page.next_offset
    }
  } catch {
    /* transient — next tick retries */
  }
}
watch(following, (on) => {
  if (on && !timer) timer = setInterval(() => void poll(), 2000)
  if (!on && timer) {
    clearInterval(timer)
    timer = null
  }
})
watch(selected, () => void reload())

function formatBytes(b: number): string {
  if (b < 1024) return `${b} B`
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`
  return `${(b / (1024 * 1024)).toFixed(1)} MB`
}

onMounted(() => void reload())
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>
