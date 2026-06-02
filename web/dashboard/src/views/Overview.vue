<template>
  <div>
    <!-- Connection error banner -->
    <v-slide-y-transition>
      <v-card v-if="!conn.connected.value" color="error" variant="tonal" class="mb-4 pa-4">
        <div class="d-flex align-center ga-3">
          <v-icon>mdi-connection</v-icon>
          <div class="flex-grow-1">
            <div class="font-weight-medium">{{ t('status.disconnected') }}</div>
            <div class="text-caption">{{ conn.lastError.value }}</div>
          </div>
          <v-btn variant="tonal" size="small" @click="retryConnection">
            {{ t('common.retry') }}
          </v-btn>
        </div>
      </v-card>
    </v-slide-y-transition>

    <!-- Metric cards -->
    <div class="d-flex flex-wrap ga-3 mb-6 card-stagger">
      <div style="flex: 1; min-width: 140px">
        <MetricCard
          :label="t('overview.running')"
          :value="jobs.totalRunning"
          icon="mdi-play-circle"
          color="warning"
          :active="activeFilter === 'running'"
          @click="toggleFilter('running')"
        />
      </div>
      <div style="flex: 1; min-width: 140px">
        <MetricCard
          :label="t('overview.pending')"
          :value="jobs.totalPending"
          icon="mdi-clock-outline"
          color="info"
          :active="activeFilter === 'pending'"
          @click="toggleFilter('pending')"
        />
      </div>
      <div v-if="config.features.gpu_map" style="flex: 1; min-width: 140px">
        <MetricCard
          :label="t('overview.gpu_free')"
          :value="gpuDisplay"
          icon="mdi-memory"
          color="success"
          :subtitle="gpuSubtitle"
        />
      </div>
      <div style="flex: 1; min-width: 140px">
        <MetricCard
          :label="t('overview.failed')"
          :value="jobs.totalFailed"
          icon="mdi-alert-circle"
          color="error"
          :active="activeFilter === 'failed'"
          @click="toggleFilter('failed')"
        />
      </div>
    </div>

    <!-- Active filter bar -->
    <v-slide-y-transition>
      <v-card v-if="activeFilter" class="mb-4 pa-3">
        <div class="d-flex align-center justify-space-between">
          <div class="d-flex align-center ga-2">
            <StatusBadge :status="activeFilter" />
            <span class="text-body-2">{{ filteredJobs.length }} {{ t('project.jobs').toLowerCase() }}</span>
          </div>
          <v-btn size="small" variant="text" @click="activeFilter = ''">
            <v-icon start size="16">mdi-close</v-icon>
            {{ t('common.clear') }}
          </v-btn>
        </div>
        <div class="d-flex flex-column ga-1 mt-2">
          <div
            v-for="j in filteredJobs.slice(0, 10)"
            :key="j.id"
            class="d-flex align-center pa-2 rounded-lg cursor-pointer hover-bg"
            @click="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
          >
            <code class="text-caption mr-3">{{ j.id.slice(0, 8) }}</code>
            <span class="text-body-2 flex-grow-1">{{ j.project }}</span>
            <span class="text-caption text-on-surface-variant">{{ j.note }}</span>
          </div>
        </div>
      </v-card>
    </v-slide-y-transition>

    <!-- GPU bars -->
    <v-slide-y-transition>
      <v-card v-if="config.features.gpu_map && gpu.gpus.length > 0 && !activeFilter" class="mb-6 pa-4">
        <div class="text-subtitle-2 mb-3">{{ t('nav.gpu') }}</div>
        <div class="d-flex flex-column ga-2">
          <GPUBar v-for="g in gpu.gpus" :key="g.index" :slot="g" />
        </div>
      </v-card>
    </v-slide-y-transition>

    <!-- Recent jobs -->
    <div v-if="recentJobs.length > 0 && !activeFilter" class="mb-6">
      <div class="text-subtitle-2 mb-3">{{ t('overview.recent') }}</div>
      <div class="d-flex flex-column ga-2 card-stagger">
        <v-card
          v-for="j in recentJobs"
          :key="j.id"
          class="pa-4 cursor-pointer"
          hover
          @click="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
        >
          <div class="d-flex align-center ga-3">
            <StatusBadge :status="j.status" />
            <div class="flex-grow-1">
              <div class="d-flex align-center ga-2">
                <span class="font-weight-medium">{{ j.project }}</span>
                <code class="text-caption text-on-surface-variant">{{ j.id.slice(0, 8) }}</code>
              </div>
              <div v-if="j.note" class="text-caption text-on-surface-variant">{{ j.note }}</div>
            </div>
            <div class="text-right">
              <div class="d-flex align-center ga-1">
                <v-progress-linear
                  :model-value="j.tasks.total > 0 ? (j.tasks.completed / j.tasks.total) * 100 : 0"
                  color="success"
                  height="4"
                  rounded
                  style="width: 60px"
                />
                <span class="text-caption text-on-surface-variant">{{ j.tasks.completed }}/{{ j.tasks.total }}</span>
              </div>
              <div class="text-caption text-on-surface-variant">{{ relativeTime(j.created_at) }}</div>
            </div>
          </div>
        </v-card>
      </div>
    </div>

    <!-- Project cards -->
    <div v-if="jobs.projects.length > 0 && !activeFilter">
      <div class="text-subtitle-2 mb-3">{{ t('overview.projects') }}</div>
      <v-row class="card-stagger">
        <v-col v-for="proj in jobs.projects" :key="proj" cols="12" sm="6" md="4">
          <v-card
            class="pa-4 cursor-pointer"
            hover
            @click="router.push({ name: 'project', params: { project: proj } })"
          >
            <div class="d-flex align-center justify-space-between mb-2">
              <span class="text-subtitle-1 font-weight-medium">{{ proj }}</span>
              <StatusBadge v-if="latestJobStatus(proj)" :status="latestJobStatus(proj)!" />
            </div>
            <div class="text-caption text-on-surface-variant">
              {{ projectJobCount(proj) }} {{ t('project.jobs').toLowerCase() }}
            </div>
          </v-card>
        </v-col>
      </v-row>
    </div>

    <!-- Empty state -->
    <v-card v-if="jobs.projects.length === 0 && !jobs.loading && !activeFilter" class="pa-8 text-center">
      <ChibiMascot :size="96" mood="thinking" variant="sparkle" class="mb-2" />
      <v-icon v-if="!settings.animeMode" size="48" color="primary" class="mb-3" style="opacity: 0.5">mdi-rocket-launch-outline</v-icon>
      <div class="text-h6 mb-1">{{ t('overview.no_projects') }}</div>
      <div class="text-body-2 text-on-surface-variant mb-4">{{ t('overview.no_projects_hint') }}</div>
      <div class="d-flex justify-center ga-2">
        <v-btn color="primary" variant="tonal" :to="{ name: 'submit' }">
          <v-icon start>mdi-plus</v-icon>
          {{ t('nav.submit') }}
        </v-btn>
      </div>
      <div class="mt-4 text-caption text-on-surface-variant">
        {{ t('overview.hint_cli') }}
        <code>runq submit train.py --lr 0.001</code>
      </div>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useJobsStore } from '@/stores/jobs'
import { useGPUStore } from '@/stores/gpu'
import { useConfigStore } from '@/stores/config'
import { usePolling } from '@/composables/usePolling'
import { useConnection } from '@/composables/useConnection'
import MetricCard from '@/components/MetricCard.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GPUBar from '@/components/GPUBar.vue'
import ChibiMascot from '@/components/ChibiMascot.vue'
import { useSettingsStore } from '@/stores/settings'

const settings = useSettingsStore()

const { t } = useI18n()
const router = useRouter()
const jobs = useJobsStore()
const gpu = useGPUStore()
const config = useConfigStore()

const conn = useConnection()
const activeFilter = ref('')

function retryConnection() {
  jobs.fetchJobs()
  if (config.features.gpu_map) gpu.fetchGPU()
}

const filteredJobs = computed(() => {
  if (!activeFilter.value) return []
  return jobs.jobs.filter(j => {
    if (activeFilter.value === 'running') return j.tasks.running > 0
    if (activeFilter.value === 'pending') return j.tasks.pending > 0
    if (activeFilter.value === 'failed') return j.tasks.failed > 0
    return false
  })
})

const recentJobs = computed(() => {
  return [...jobs.jobs]
    .sort((a, b) => b.created_at - a.created_at)
    .slice(0, 5)
})

const gpuDisplay = computed(() => {
  if (gpu.totalCount === 0) return '—'
  return `${gpu.freeCount}/${gpu.totalCount}`
})

const gpuSubtitle = computed(() => {
  if (gpu.totalCount === 0) return t('status.gpu_off')
  return undefined
})

function toggleFilter(status: string) {
  activeFilter.value = activeFilter.value === status ? '' : status
}

function projectJobCount(proj: string) {
  return jobs.jobsByProject.get(proj)?.length ?? 0
}

function latestJobStatus(proj: string): string | undefined {
  const arr = jobs.jobsByProject.get(proj)
  if (!arr || arr.length === 0) return undefined
  return arr[0].status
}

function relativeTime(ts: number): string {
  const diff = Date.now() / 1000 - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

// Polling — pass silent flag to suppress toasts on interval ticks
usePolling((silent: boolean) => {
  jobs.fetchJobs(silent)
  if (config.features.gpu_map) gpu.fetchGPU(silent)
}, 5000)
</script>

<style scoped>
.hover-bg:hover {
  background: rgb(var(--v-theme-surface-variant));
  transition: background 0.15s ease;
}
</style>
