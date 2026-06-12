<template>
  <div>
    <!-- Connection error -->
    <v-slide-y-transition>
      <v-card v-if="!conn.connected.value" color="error" variant="tonal" class="mb-4 pa-3">
        <div class="d-flex align-center ga-2">
          <v-icon size="18">{{ conn.daemonDown.value ? 'mdi-server-off' : 'mdi-connection' }}</v-icon>
          <div class="flex-grow-1">
            <div class="text-body-2 font-weight-medium">{{ t(conn.statusKey.value) }}</div>
            <div v-if="conn.daemonDown.value" class="text-caption d-flex align-center ga-1">
              <span>{{ t('status.daemon_down_hint') }}</span>
              <code class="px-1">runq daemon start --detach</code>
            </div>
            <div v-else class="text-caption">{{ conn.lastError.value }}</div>
          </div>
          <v-btn variant="tonal" size="x-small" @click="retryConnection">{{ t('common.retry') }}</v-btn>
        </div>
      </v-card>
    </v-slide-y-transition>

    <!-- Metric row -->
    <v-row dense class="mb-4">
      <v-col v-for="m in metrics" :key="m.key" cols="6" sm="3">
        <v-card
          class="pa-3 cursor-pointer"
          :class="{ 'border-primary': activeFilter === m.key }"
          @click="toggleFilter(m.key)"
        >
          <div class="d-flex align-center justify-space-between">
            <div>
              <div class="text-caption text-on-surface-variant">{{ m.label }}</div>
              <div class="text-h5 font-weight-medium">{{ m.value }}</div>
            </div>
            <StatusDot :status="m.status" :size="10" />
          </div>
        </v-card>
      </v-col>
    </v-row>

    <!-- GPU bars (always visible in daemon mode) -->
    <v-card v-if="config.caps.gpu_map && gpu.gpus.length > 0" class="mb-4 pa-3">
      <div class="text-caption text-on-surface-variant mb-2">GPU</div>
      <div class="d-flex flex-column ga-1">
        <GPUBar v-for="g in gpu.gpus" :key="g.index" :slot="g" />
      </div>
    </v-card>

    <!-- Active filter: show matching jobs -->
    <v-card v-if="activeFilter" class="mb-4 pa-3">
      <div class="d-flex align-center justify-space-between mb-2">
        <div class="d-flex align-center ga-2">
          <StatusDot
            :status="activeFilter === 'done' ? 'done' : activeFilter"
            :kind="activeFilter === 'done' ? 'job' : 'task'"
          />
          <span class="text-body-2 font-weight-medium">{{ filteredJobs.length }} jobs</span>
        </div>
        <v-btn size="x-small" variant="text" @click="activeFilter = ''">
          <v-icon size="14">mdi-close</v-icon> Clear
        </v-btn>
      </div>
      <div class="overflow-x-auto">
        <table class="data-mono" style="width: 100%">
          <thead><tr><th>ID</th><th>Project</th><th>Note</th><th>Tasks</th><th>Created</th></tr></thead>
          <tbody>
            <tr
              v-for="j in filteredJobs.slice(0, 20)"
              :key="j.id"
              class="cursor-pointer"
              @click="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
            >
              <td><code>{{ j.id.slice(0, 8) }}</code></td>
              <td>{{ j.project }}</td>
              <td class="text-on-surface-variant">{{ j.note || '—' }}</td>
              <td>{{ j.tasks.completed }}/{{ j.tasks.total }}</td>
              <td class="text-on-surface-variant">{{ relativeTime(j.created_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </v-card>

    <!-- Recent jobs table -->
    <div v-if="!activeFilter && recentJobs.length > 0" class="mb-4">
      <div class="text-subtitle-2 mb-2">{{ t('overview.recent') }}</div>
      <v-card class="pa-0">
        <div class="overflow-x-auto">
          <table class="data-mono" style="width: 100%">
            <thead><tr><th></th><th>ID</th><th>Project</th><th>Note</th><th>Progress</th><th>Created</th></tr></thead>
            <tbody>
              <tr
                v-for="j in recentJobs"
                :key="j.id"
                class="cursor-pointer"
                @click="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
              >
                <td style="width: 24px"><StatusDot :status="j.status" kind="job" /></td>
                <td><code>{{ j.id.slice(0, 8) }}</code></td>
                <td class="font-weight-medium">{{ j.project }}</td>
                <td class="text-on-surface-variant">{{ j.note || '—' }}</td>
                <td>
                  <div class="d-flex align-center ga-2">
                    <v-progress-linear
                      :model-value="j.tasks.total > 0 ? (j.tasks.completed / j.tasks.total) * 100 : 0"
                      color="success" height="3" rounded style="width: 50px"
                    />
                    <span class="text-on-surface-variant">{{ j.tasks.completed }}/{{ j.tasks.total }}</span>
                  </div>
                </td>
                <td class="text-on-surface-variant">{{ relativeTime(j.created_at) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </v-card>
    </div>

    <!-- Empty state -->
    <v-card v-if="jobs.projects.length === 0 && !jobs.loading && !activeFilter" class="pa-8 text-center">
      <v-icon size="40" color="primary" class="mb-3" style="opacity: 0.4">mdi-rocket-launch-outline</v-icon>
      <div class="text-h6 mb-1">{{ t('overview.no_projects') }}</div>
      <div class="text-body-2 text-on-surface-variant mb-4">{{ t('overview.no_projects_hint') }}</div>
      <v-btn color="primary" variant="tonal" :to="{ name: 'submit' }">
        <v-icon start size="16">mdi-plus</v-icon> {{ t('nav.submit') }}
      </v-btn>
      <div class="mt-4 text-caption text-on-surface-variant">
        {{ t('overview.hint_cli') }} <code>runq init train.py</code> → <code>runq submit .</code>
      </div>
    </v-card>

    <!-- Archived projects: the recovery entry point — once a project is
         archived its jobs cascade-hide, so without this row it would have
         no discoverable way back. -->
    <v-card v-if="archivedProjects.length > 0" class="mt-3">
      <div class="d-flex align-center ga-2 px-4 py-3 cursor-pointer text-on-surface-variant" @click="archivedOpen = !archivedOpen">
        <v-icon size="16">mdi-archive-outline</v-icon>
        <span class="text-subtitle-2">{{ t('archive.projects_section', { n: archivedProjects.length }) }}</span>
        <v-spacer />
        <v-icon size="16">{{ archivedOpen ? 'mdi-chevron-up' : 'mdi-chevron-down' }}</v-icon>
      </div>
      <div v-if="archivedOpen" class="px-2 pb-2">
        <div
          v-for="p in archivedProjects" :key="p.name"
          class="d-flex align-center ga-2 px-2 py-1 rounded cursor-pointer"
          @click="router.push({ name: 'project', params: { project: p.name } })"
        >
          <v-icon size="14" color="on-surface-variant">mdi-folder-outline</v-icon>
          <span class="text-body-2">{{ p.name }}</span>
          <span class="text-caption text-on-surface-variant">{{ p.job_count }} jobs</span>
          <v-spacer />
          <v-btn size="x-small" variant="text" @click.stop="unarchiveProject(p.name)">
            <v-icon start size="12">mdi-archive-arrow-up-outline</v-icon> {{ t('archive.unarchive') }}
          </v-btn>
        </div>
      </div>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useJobsStore } from '@/stores/jobs'
import { useProjectStore } from '@/stores/projects'
import { useGPUStore } from '@/stores/gpu'
import { useConfigStore } from '@/stores/config'
import { usePolling } from '@/composables/usePolling'
import { useConnection } from '@/composables/useConnection'
import GPUBar from '@/components/GPUBar.vue'
import StatusDot from '@/components/StatusDot.vue'
import { useSnackbar } from '@/composables/useSnackbar'

const { t } = useI18n()
const router = useRouter()
const jobs = useJobsStore()
const projectStore = useProjectStore()
const gpu = useGPUStore()
const config = useConfigStore()
const conn = useConnection()
const snack = useSnackbar()

// ── Archived projects (recovery entry) — derived from the store ──
const archivedOpen = ref(false)
const archivedProjects = computed(() => projectStore.archived)

async function unarchiveProject(name: string) {
  try {
    await projectStore.unarchive(name) // store action owns ALL refreshes
    snack.success(t('archive.project_back'))
  } catch (e: any) { snack.error(e?.message || 'Unarchive failed') }
}
const activeFilter = ref('')

const metrics = computed(() => [
  { key: 'running', label: t('overview.running'), value: jobs.totalRunning, status: 'running' },
  { key: 'pending', label: t('overview.pending'), value: jobs.totalPending, status: 'pending' },
  { key: 'failed', label: t('overview.failed'), value: jobs.totalFailed, status: 'failed' },
  { key: 'done', label: t('overview.completed'), value: totalCompleted.value, status: 'success' },
])

const totalCompleted = computed(() =>
  jobs.jobs.reduce((sum, job) => sum + job.tasks.completed, 0)
)

function retryConnection() {
  jobs.fetchJobs()
  if (config.caps.gpu_map) gpu.fetchGPU()
}

const filteredJobs = computed(() => {
  if (!activeFilter.value) return []
  return jobs.jobs.filter(j => {
    if (activeFilter.value === 'running') return j.tasks.running > 0
    if (activeFilter.value === 'pending') return j.tasks.pending > 0
    if (activeFilter.value === 'failed') return j.tasks.failed > 0
    if (activeFilter.value === 'done') return j.status === 'done'
    return false
  })
})

const recentJobs = computed(() =>
  [...jobs.jobs].sort((a, b) => b.created_at - a.created_at).slice(0, 10)
)

function toggleFilter(status: string) {
  activeFilter.value = activeFilter.value === status ? '' : status
}

function relativeTime(ts: number): string {
  const diff = Date.now() / 1000 - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

usePolling((silent: boolean) => {
  jobs.fetchJobs(silent)
  projectStore.fetch()
  if (config.caps.gpu_map) gpu.fetchGPU(silent)
}, 5000)
</script>

<style scoped>
.border-primary {
  border: 1.5px solid rgb(var(--v-theme-primary)) !important;
}
</style>
