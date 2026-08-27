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

    <!-- First-load skeleton: metric cards + table rows instead of a blank page -->
    <template v-if="jobsLoading">
      <v-row dense class="mb-4">
        <v-col v-for="i in 4" :key="i" cols="6" sm="3">
          <v-card class="pa-3">
            <v-skeleton-loader type="text" width="60%" />
            <v-skeleton-loader type="heading" width="40%" />
          </v-card>
        </v-col>
      </v-row>
      <v-card class="pa-0">
        <v-skeleton-loader type="table-row@6" />
      </v-card>
    </template>

    <!-- ── Needs attention (RQ2-4 ⑤, kit ScreensA): the only block on
         this page allowed alarm colour, and the only one that disappears
         when there is nothing to say. Acknowledging is not deleting —
         the ack is stored against a SIGNATURE of the reported state, so
         a seen row stays quiet until the situation changes. ── -->
    <v-card v-if="!jobsLoading && (openAttention.length > 0 || ackedCount > 0)" class="mb-4 pa-0">
      <div class="d-flex align-baseline ga-2 px-4 pt-3 pb-2">
        <span class="text-subtitle-2">{{ t('overview.attn_title') }}</span>
        <span class="text-caption text-on-surface-variant">{{ t('overview.attn_window') }}</span>
        <v-spacer />
        <span
          v-if="ackedCount > 0"
          class="text-caption text-on-surface-variant cursor-pointer attn-show-acked"
          role="button" tabindex="0"
          @click="unackAll"
          @keydown.enter="unackAll"
        >{{ t('overview.attn_show_acked', { n: ackedCount }) }}</span>
      </div>
      <div v-if="openAttention.length === 0" class="px-4 py-2 text-body-2 text-on-surface-variant attn-row-border">
        {{ t('overview.attn_clear') }}
      </div>
      <div
        v-for="a in openAttention" :key="a.key"
        class="attn-row d-flex align-center ga-2 px-4 py-2 attn-row-border"
      >
        <v-icon size="14" :color="a.tone" class="flex-shrink-0">{{ a.icon }}</v-icon>
        <span
          class="text-body-2 cursor-pointer flex-shrink-0"
          role="link" tabindex="0"
          @click="openJob(a.job.project, a.job.id)"
          @keydown.enter="openJob(a.job.project, a.job.id)"
        >{{ t(a.textKey, a.textParams) }}</span>
        <span class="text-caption text-on-surface-variant font-mono text-truncate flex-grow-1">{{ a.job.note }}</span>
        <v-btn
          icon size="x-small" variant="text" density="comfortable"
          class="attn-ack flex-shrink-0"
          :title="t('overview.attn_ack_tip')"
          :aria-label="t('overview.attn_ack_tip')"
          @click="ack(a.key, a.sig)"
        >
          <v-icon size="13">mdi-check</v-icon>
        </v-btn>
        <v-icon size="14" color="on-surface-variant" class="flex-shrink-0">mdi-chevron-right</v-icon>
      </div>
    </v-card>

    <!-- ── Targets × job status: where the work actually is, and the
         page's filter control (click a number to filter the list below).
         Daemon targets carry a free/total GPU chip; scheduler targets
         have no such visibility and honestly show none. ── -->
    <v-card v-if="!jobsLoading && matrix.rows.length > 0" class="mb-4 pa-0">
      <div class="d-flex align-baseline ga-2 px-4 pt-3 pb-2">
        <span class="text-subtitle-2">{{ t('overview.targets_title') }}</span>
        <span class="text-caption text-on-surface-variant">{{ t('overview.targets_hint') }}</span>
      </div>
      <div class="overflow-x-auto">
        <table class="data-mono" style="width: 100%">
          <thead>
            <tr>
              <th>{{ t('overview.target_col') }}</th>
              <th v-for="s in matrix.statusCols" :key="s" class="text-right">
                <span class="d-inline-flex align-center ga-1">
                  <StatusDot :status="s" kind="job" :size="6" />{{ t('status.job.' + s) }}
                </span>
              </th>
              <th class="text-right">{{ t('overview.jobs_col') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in matrix.rows" :key="r.name">
              <td>
                <span class="d-inline-flex align-center ga-2">
                  <button
                    type="button" class="cell-btn font-weight-medium"
                    :class="{ 'text-primary': filter.target === r.name && !filter.status }"
                    @click="filter = toggleCell(filter, r.name, '')"
                  >{{ r.name }}</button>
                  <v-chip v-if="r.gpus" size="x-small" variant="tonal" label :title="t('overview.gpu_chip_tip')">
                    {{ r.gpus.free }}/{{ r.gpus.total }} GPU
                  </v-chip>
                  <span v-else-if="r.scheduler" class="text-caption text-on-surface-variant">{{ r.scheduler }}</span>
                </span>
              </td>
              <td v-for="s in matrix.statusCols" :key="s" class="text-right">
                <button
                  v-if="r.counts[s]"
                  type="button" class="cell-btn font-weight-medium"
                  :class="cellClass(r.name, s)"
                  @click="filter = toggleCell(filter, r.name, s)"
                >{{ r.counts[s] }}</button>
                <span v-else class="text-on-surface-variant">0</span>
              </td>
              <td class="text-right">
                <button
                  type="button" class="cell-btn font-weight-medium"
                  :class="{ 'text-primary': filter.target === r.name && !filter.status }"
                  @click="filter = toggleCell(filter, r.name, '')"
                >{{ r.total }}</button>
              </td>
            </tr>
            <!-- The whole cluster on one line, same click semantics -->
            <tr v-if="matrix.rows.length > 1" class="grand-row">
              <td class="text-on-surface-variant">{{ t('overview.all_targets') }}</td>
              <td v-for="s in matrix.statusCols" :key="s" class="text-right">
                <button
                  v-if="matrix.grand[s]"
                  type="button" class="cell-btn font-weight-medium"
                  :class="cellClass('', s)"
                  @click="filter = toggleCell(filter, '', s)"
                >{{ matrix.grand[s] }}</button>
                <span v-else class="text-on-surface-variant">0</span>
              </td>
              <td class="text-right font-weight-medium">{{ jobList.length }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </v-card>

    <!-- Recent jobs table (filtered by the matrix selection) -->
    <div v-if="!jobsLoading && recentJobs.length > 0" class="mb-4">
      <div class="d-flex align-center ga-2 mb-2">
        <span class="text-subtitle-2">
          {{ hasFilter ? t('overview.n_jobs', { n: filteredJobs.length }) : t('overview.recent') }}
        </span>
        <v-chip
          v-if="filter.target" size="small" variant="tonal" color="primary" closable
          @click:close="filter = { ...filter, target: '' }"
        >{{ filter.target }}</v-chip>
        <v-chip
          v-if="filter.status" size="small" variant="tonal" color="primary" closable
          @click:close="filter = { ...filter, status: '' }"
        >
          <StatusDot :status="filter.status" kind="job" :size="6" class="mr-1" />{{ t('status.job.' + filter.status) }}
        </v-chip>
      </div>
      <v-card class="pa-0">
        <div class="overflow-x-auto">
          <table class="data-mono" style="width: 100%">
            <thead><tr><th></th><th>ID</th><th>{{ t('table.project') }}</th><th>{{ t('table.note') }}</th><th>{{ t('table.progress') }}</th><th>{{ t('table.created') }}</th></tr></thead>
            <tbody>
              <tr
                v-for="j in recentJobs"
                :key="j.id"
                class="cursor-pointer"
                tabindex="0"
                role="link"
                :aria-label="t('a11y.open_job', { id: j.id.slice(0, 8) })"
                @click="openJob(j.project, j.id)"
                @keydown.enter="openJob(j.project, j.id)"
                @keydown.space.prevent="openJob(j.project, j.id)"
              >
                <td style="width: 24px"><StatusDot :status="j.status" kind="job" :size="14" /></td>
                <td><code>{{ j.id.slice(0, 8) }}</code></td>
                <td class="font-weight-medium">{{ j.project }}</td>
                <td class="text-on-surface-variant">{{ j.note || '—' }}</td>
                <td>
                  <div class="d-flex align-center ga-2">
                    <SegmentedProgress :counts="j.tasks" :height="3" style="width: 50px" />
                    <span class="text-on-surface-variant">{{ j.tasks.completed }}/{{ j.tasks.total }}</span>
                    <span v-if="j.tasks.failed > 0" class="text-error">· {{ t('job.n_failed', { n: j.tasks.failed }) }}</span>
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
    <v-card v-if="jobList.length === 0 && !jobsLoading" class="pa-8 text-center">
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
      <div
        class="d-flex align-center ga-2 px-4 py-3 cursor-pointer text-on-surface-variant"
        role="button" tabindex="0" :aria-expanded="archivedOpen"
        @click="archivedOpen = !archivedOpen"
        @keydown.enter="archivedOpen = !archivedOpen"
        @keydown.space.prevent="archivedOpen = !archivedOpen"
      >
        <v-icon size="16">mdi-archive-outline</v-icon>
        <span class="text-subtitle-2">{{ t('archive.projects_section', { n: archivedProjects.length }) }}</span>
        <v-spacer />
        <v-icon size="16">{{ archivedOpen ? 'mdi-chevron-up' : 'mdi-chevron-down' }}</v-icon>
      </div>
      <div v-if="archivedOpen" class="px-2 pb-2">
        <div
          v-for="p in archivedProjects" :key="p.name"
          class="d-flex align-center ga-2 px-2 py-1 rounded cursor-pointer row-focus"
          role="link" tabindex="0"
          :aria-label="t('a11y.open_project', { name: p.name })"
          @click="router.push({ name: 'project', params: { project: p.name } })"
          @keydown.enter="router.push({ name: 'project', params: { project: p.name } })"
          @keydown.space.prevent="router.push({ name: 'project', params: { project: p.name } })"
        >
          <v-icon size="14" color="on-surface-variant">mdi-folder-outline</v-icon>
          <span class="text-body-2">{{ p.name }}</span>
          <span class="text-caption text-on-surface-variant">{{ t('project.job_count', p.job_count) }}</span>
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
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useProjectStore } from '@/stores/projects'
import { useConfigStore } from '@/stores/config'
import { useConnection } from '@/composables/useConnection'
import { useJobsListQuery } from '@/queries/useJobQueries'
import { useGpuQuery } from '@/queries/useGpuQuery'
import StatusDot from '@/components/StatusDot.vue'
import SegmentedProgress from '@/components/SegmentedProgress.vue'
import { useSnackbar } from '@/composables/useSnackbar'
import { relativeTime } from '@/utils/relativeTime'
import {
  attentionItems, splitAcked, targetMatrix, toggleCell, applyFilter,
  type MatrixFilter,
} from './overviewView'

const { t } = useI18n()
const router = useRouter()
const projectStore = useProjectStore()
const config = useConfigStore()
const conn = useConnection()
const snack = useSnackbar()

// ── Server state: polling/dedupe/freshness live in the query layer. ──
const jobsQuery = useJobsListQuery()
const jobList = computed(() => jobsQuery.data.value ?? [])
const jobsLoading = computed(() => jobsQuery.isLoading.value)
const gpuQuery = useGpuQuery()
const gpus = computed(() => gpuQuery.data.value ?? [])

// Project list is low-churn: load on mount, mutations refresh it themselves.
onMounted(() => { projectStore.fetch() })

// ── Archived projects (recovery entry) — derived from the store ──
const archivedOpen = ref(false)
const archivedProjects = computed(() => projectStore.archived)

async function unarchiveProject(name: string) {
  try {
    await projectStore.unarchive(name) // store action owns ALL refreshes
    snack.success(t('archive.project_back'))
  } catch (e: any) { snack.error(e?.message || t('common.error')) }
}
function retryConnection() {
  jobsQuery.refetch()
  gpuQuery.refetch()
}

function openJob(project: string, id: string) {
  router.push({ name: 'job-detail', params: { project, jobId: id } })
}

// ── Needs attention (RQ2-4 ⑤): acks live in localStorage keyed by job,
// valued by the state SIGNATURE — a changed situation reopens the row. ──
const ACK_KEY = 'runq-acked'
const acked = ref<Record<string, string>>(readAcked())
function readAcked(): Record<string, string> {
  try {
    const raw = JSON.parse(localStorage.getItem(ACK_KEY) || '{}')
    return typeof raw === 'object' && raw !== null ? raw : {}
  } catch {
    return {}
  }
}
function ack(key: string, sig: string) {
  acked.value = { ...acked.value, [key]: sig }
  localStorage.setItem(ACK_KEY, JSON.stringify(acked.value))
}
function unackAll() {
  acked.value = {}
  localStorage.removeItem(ACK_KEY)
}

// `now` re-evaluates with every poll (jobList is in the dependency
// chain), which is exactly the cadence the 24h window needs.
const attention = computed(() => attentionItems(jobList.value, Math.floor(Date.now() / 1000)))
const openAttention = computed(() => splitAcked(attention.value, acked.value).open)
const ackedCount = computed(() => splitAcked(attention.value, acked.value).ackedCount)

// ── Targets × status matrix + the one two-axis filter ──
const matrix = computed(() => targetMatrix(jobList.value, config.targets, gpus.value))
const filter = ref<MatrixFilter>({ target: '', status: '' })
const hasFilter = computed(() => !!filter.value.target || !!filter.value.status)

function cellClass(target: string, status: string): string {
  const on = filter.value.target === target && filter.value.status === status
  return on ? 'text-primary cell-on' : ''
}

const filteredJobs = computed(() => applyFilter(jobList.value, filter.value))
/** Filtered → everything that matches; unfiltered → the newest 10. */
const recentJobs = computed(() =>
  hasFilter.value ? filteredJobs.value : filteredJobs.value.slice(0, 10))
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.text-right { text-align: right; }

/* Needs attention: the ack affordance appears on hover — a row you are
   not looking at should not advertise a dismiss button. */
.attn-row-border { border-top: 0.5px solid rgb(var(--v-theme-outline-variant), 0.6); }
.attn-ack { opacity: 0; transition: opacity 0.15s ease; }
.attn-row:hover .attn-ack,
.attn-ack:focus-visible { opacity: 1; }
.attn-show-acked {
  text-decoration: underline dotted;
  text-underline-offset: 3px;
}

/* Matrix cells are buttons — keep the mono table look. */
.cell-btn {
  background: none;
  border: none;
  padding: 1px 4px;
  border-radius: 4px;
  font: inherit;
  color: inherit;
  cursor: pointer;
}
.cell-btn:hover { background: rgb(var(--v-theme-surface-variant), 0.5); }
.cell-on { background: rgb(var(--v-theme-primary), 0.1); }
.grand-row td { border-top: 1px solid rgb(var(--v-theme-outline-variant)); }
</style>
