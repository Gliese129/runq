<template>
  <div>
    <!-- Header -->
    <div class="d-flex align-center justify-space-between mb-4">
      <div class="text-h5 font-weight-bold">{{ project }}</div>
      <div class="d-flex ga-2">
        <v-btn variant="text" size="small" :to="{ name: 'project-edit', params: { project } }">
          <v-icon start size="16">mdi-pencil-outline</v-icon>
          {{ t('projectEdit.edit_project') }}
        </v-btn>
        <v-btn variant="text" size="small"
          :title="projectArchived ? undefined : t('archive.project_tooltip')"
          @click="toggleProjectArchive"
        >
          <v-icon start size="16">{{ projectArchived ? 'mdi-archive-arrow-up-outline' : 'mdi-archive-arrow-down-outline' }}</v-icon>
          {{ projectArchived ? t('archive.unarchive') : t('archive.archive') }}
        </v-btn>
        <v-btn variant="tonal" color="primary" size="small" :to="{ name: 'submit' }">
          <v-icon start size="16">mdi-plus</v-icon> {{ t('submit.new_job') }}
        </v-btn>
      </div>
    </div>

    <!-- Filters -->
    <div class="d-flex align-center ga-2 mb-3 flex-wrap">
      <!-- role=button + aria-pressed: VChip is a focusable span and already
           handles Enter/Space itself; SR users just need the toggle semantics. -->
      <v-chip
        v-for="s in statusFilters"
        :key="s.value"
        role="button"
        :aria-pressed="statusFilter === s.value"
        :variant="statusFilter === s.value ? 'flat' : 'outlined'"
        :color="statusFilter === s.value ? 'primary' : undefined"
        size="small"
        @click="statusFilter = statusFilter === s.value ? '' : s.value"
      >
        <StatusDot v-if="s.dot" :status="s.dot" :kind="s.kind" :size="6" class="mr-1" />
        {{ s.label }}
      </v-chip>
      <v-spacer />
      <v-text-field
        v-model="searchQuery"
        :placeholder="t('project.search_note')"
        prepend-inner-icon="mdi-magnify"
        density="compact"
        variant="outlined"
        hide-details
        single-line
        clearable
        style="max-width: 200px"
      />
    </div>

    <!-- Jobs table -->
    <v-card v-if="displayedJobs.length > 0" class="pa-0">
      <div class="overflow-x-auto">
        <table class="data-mono" style="width: 100%">
          <thead>
            <tr>
              <th style="width: 24px"></th>
              <!-- th keeps its columnheader role (aria-sort lives there);
                   the inner <button> supplies keyboard access and focus. -->
              <th :aria-sort="ariaSort('id')">
                <button type="button" class="th-sort-btn" @click="toggleSort('id')">
                  ID {{ sortIcon('id') }}
                </button>
              </th>
              <th :aria-sort="ariaSort('note')">
                <button type="button" class="th-sort-btn" @click="toggleSort('note')">
                  {{ t('table.note') }} {{ sortIcon('note') }}
                </button>
              </th>
              <th :aria-sort="ariaSort('tasks')">
                <button type="button" class="th-sort-btn" @click="toggleSort('tasks')">
                  {{ t('table.tasks') }} {{ sortIcon('tasks') }}
                </button>
              </th>
              <th :aria-sort="ariaSort('created_at')">
                <button type="button" class="th-sort-btn" @click="toggleSort('created_at')">
                  {{ t('table.created') }} {{ sortIcon('created_at') }}
                </button>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="j in displayedJobs"
              :key="j.id"
              class="cursor-pointer"
              tabindex="0"
              role="link"
              :aria-label="t('a11y.open_job', { id: j.id.slice(0, 8) })"
              @click="router.push({ name: 'job-detail', params: { project, jobId: j.id } })"
              @keydown.enter="router.push({ name: 'job-detail', params: { project, jobId: j.id } })"
              @keydown.space.prevent="router.push({ name: 'job-detail', params: { project, jobId: j.id } })"
            >
              <td><StatusDot :status="j.status" kind="job" :size="14" /></td>
              <td><code>{{ j.id.slice(0, 8) }}</code></td>
              <td class="text-on-surface-variant">{{ j.note || '—' }}</td>
              <td>
                <div class="d-flex align-center ga-2">
                  <SegmentedProgress :counts="j.tasks" :height="3" style="width: 40px" />
                  {{ j.tasks.completed }}/{{ j.tasks.total }}
                  <span v-if="j.tasks.failed > 0" class="text-error">· {{ t('job.n_failed', { n: j.tasks.failed }) }}</span>
                </div>
              </td>
              <td class="text-on-surface-variant">{{ relativeTime(j.created_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </v-card>

    <!-- Empty state -->
    <v-card v-else-if="!loading" class="pa-8 text-center">
      <v-icon size="36" color="primary" class="mb-3" style="opacity: 0.4">mdi-briefcase-outline</v-icon>
      <div class="text-h6 mb-1">{{ t('project.no_jobs') }}</div>
      <div class="text-body-2 text-on-surface-variant mb-4">{{ t('project.no_jobs_hint') }}</div>
      <v-btn color="primary" variant="tonal" :to="{ name: 'submit' }">{{ t('nav.submit') }}</v-btn>
    </v-card>

    <!-- Archived jobs of this project (collapsed by default) -->
    <v-card v-if="archivedJobs.length > 0" class="mt-3">
      <div
        class="d-flex align-center ga-2 px-4 py-3 cursor-pointer text-on-surface-variant"
        role="button" tabindex="0" :aria-expanded="archivedOpen"
        @click="archivedOpen = !archivedOpen"
        @keydown.enter="archivedOpen = !archivedOpen"
        @keydown.space.prevent="archivedOpen = !archivedOpen"
      >
        <v-icon size="16">mdi-archive-outline</v-icon>
        <span class="text-subtitle-2">{{ t('archive.section', { n: archivedJobs.length }) }}</span>
        <v-spacer />
        <v-icon size="16">{{ archivedOpen ? 'mdi-chevron-up' : 'mdi-chevron-down' }}</v-icon>
      </div>
      <div v-if="archivedOpen" class="px-2 pb-2">
        <div
          v-for="j in archivedJobs" :key="j.id"
          class="d-flex align-center ga-2 px-2 py-1 rounded recent-row cursor-pointer row-focus"
          role="link" tabindex="0"
          :aria-label="t('a11y.open_job', { id: j.id.slice(0, 10) })"
          @click="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
          @keydown.enter="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
          @keydown.space.prevent="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
        >
          <code class="text-body-2">{{ j.id.slice(0, 10) }}</code>
          <span class="text-caption text-on-surface-variant text-truncate flex-grow-1">{{ j.note || '—' }}</span>
          <v-btn size="x-small" variant="text" @click.stop="unarchiveJob(j.id)">
            <v-icon start size="12">mdi-archive-arrow-up-outline</v-icon> {{ t('archive.unarchive') }}
          </v-btn>
        </div>
      </div>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useProjectStore } from '@/stores/projects'
import { useSnackbar } from '@/composables/useSnackbar'
import { useJobsListQuery, useArchivedJobsQuery, useProjectJobsQuery, useJobActions } from '@/queries/useJobQueries'
import StatusDot from '@/components/StatusDot.vue'
import SegmentedProgress from '@/components/SegmentedProgress.vue'
import type { JobSummary } from '@/types/api'
import { relativeTime } from '@/utils/relativeTime'

const props = defineProps<{ project: string }>()
const { t } = useI18n()
const projectStore = useProjectStore()
const router = useRouter()
const snack = useSnackbar()

// ── Server state: query cache is the single source. Keys derive from
// props.project, so route-param switches re-point automatically — no
// manual refetch choreography, nothing here to go stale.
const listQuery = useJobsListQuery()
const archivedQuery = useArchivedJobsQuery()
const scopedQuery = useProjectJobsQuery(() => props.project)
const jobActions = useJobActions()

const archivedOpen = ref(false)
const projectArchived = computed(() =>
  !!projectStore.list.find(p => p.name === props.project)?.archived)
const archivedJobs = computed(() =>
  (archivedQuery.data.value ?? []).filter(j => j.project === props.project))
// Inside an archived project the global list is empty BY DESIGN (cascade);
// the scoped query skips it.
const scopedJobs = computed(() => scopedQuery.data.value ?? [])
const loading = computed(() => listQuery.isLoading.value)

watch(() => props.project, () => {
  archivedOpen.value = false
  projectStore.fetch() // project archive flags live in the pinia store
}, { immediate: true })

async function toggleProjectArchive() {
  try {
    if (projectArchived.value) {
      await projectStore.unarchive(props.project)
      snack.success(t('archive.project_back'))
    } else {
      await projectStore.archive(props.project)
      snack.success(t('archive.project_done'))
    }
    await Promise.all([archivedQuery.refetch(), scopedQuery.refetch(), listQuery.refetch()])
  } catch (e: any) { snack.error(e?.message || t('common.error')) }
}

async function unarchiveJob(id: string) {
  try {
    await jobActions.unarchive.mutateAsync({ id, project: props.project })
    snack.success(t('archive.job_back'))
  } catch (e: any) { snack.error(e?.message || t('common.error')) }
}

const statusFilter = ref('')
const searchQuery = ref('')
const sortKey = ref('created_at')
const sortDesc = ref(true)

// Count-carrying filter chips: "x/n done · y/n failed · z/n running".
// The chip reports MATCH COUNTS instead of pretending to be a job-status
// taxonomy — so partial/killed jobs need no chip color of their own: the
// backend folds killed into tasks.failed, hence every not-fully-successful
// terminal job lands under "failed" naturally. "done" stays strict (job
// fully succeeded). Overlap across chips (a live job can match running AND
// failed) is inherent to contains-semantics and intended.
function matchesFilter(j: JobSummary, f: string): boolean {
  if (f === 'running') return j.tasks.running > 0
  if (f === 'pending') return j.tasks.pending > 0
  if (f === 'failed') return j.tasks.failed > 0
  if (f === 'done') return j.status === 'done'
  return true
}

const statusFilters = computed<{ value: string; label: string; dot: string; kind: 'task' | 'job' }[]>(() => {
  const n = projectJobs.value.length
  const count = (f: string) => projectJobs.value.filter(j => matchesFilter(j, f)).length
  return [
    { value: '', label: t('common.all'), dot: '', kind: 'task' },
    { value: 'done', label: t('filter.count', { x: count('done'), n, s: t('status.job.done') }), dot: 'done', kind: 'job' },
    { value: 'failed', label: t('filter.count', { x: count('failed'), n, s: t('status.task.failed') }), dot: 'failed', kind: 'task' },
    { value: 'running', label: t('filter.count', { x: count('running'), n, s: t('status.task.running') }), dot: 'running', kind: 'task' },
    { value: 'pending', label: t('filter.count', { x: count('pending'), n, s: t('status.task.pending') }), dot: 'pending', kind: 'task' },
  ]
})

const projectJobs = computed(() =>
  projectArchived.value
    ? scopedJobs.value
    : (listQuery.data.value ?? []).filter(j => j.project === props.project))

const displayedJobs = computed(() => {
  let list = [...projectJobs.value]

  if (statusFilter.value) {
    // Same predicate the chip counted with — the number IS the promise.
    list = list.filter(j => matchesFilter(j, statusFilter.value))
  }

  if (searchQuery.value) {
    const q = searchQuery.value.toLowerCase()
    list = list.filter(j => j.note?.toLowerCase().includes(q) || j.id.includes(q))
  }

  list.sort((a, b) => {
    let cmp = 0
    if (sortKey.value === 'created_at') cmp = a.created_at - b.created_at
    else if (sortKey.value === 'tasks') cmp = a.tasks.total - b.tasks.total
    else if (sortKey.value === 'note') cmp = (a.note || '').localeCompare(b.note || '')
    else if (sortKey.value === 'id') cmp = a.id.localeCompare(b.id)
    return sortDesc.value ? -cmp : cmp
  })

  return list
})

function toggleSort(key: string) {
  if (sortKey.value === key) sortDesc.value = !sortDesc.value
  else { sortKey.value = key; sortDesc.value = true }
}

function sortIcon(key: string): string {
  if (sortKey.value !== key) return ''
  return sortDesc.value ? '↓' : '↑'
}

/** aria-sort value for a sortable header (a11y). */
function ariaSort(key: string): 'ascending' | 'descending' | 'none' {
  if (sortKey.value !== key) return 'none'
  return sortDesc.value ? 'descending' : 'ascending'
}
</script>
