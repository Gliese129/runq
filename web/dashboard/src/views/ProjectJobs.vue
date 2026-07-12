<template>
  <div>
    <!-- Header -->
    <div class="d-flex align-center justify-space-between mb-4">
      <div class="text-h5 font-weight-bold">{{ project }}</div>
      <div class="d-flex ga-2">
        <v-btn variant="text" size="small"
          :title="projectArchived ? undefined : 'Hide this project and its jobs from default lists — reversible'"
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
      <v-chip
        v-for="s in statusFilters"
        :key="s.value"
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
        placeholder="Search by note..."
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
              <th class="cursor-pointer" @click="toggleSort('id')">
                ID {{ sortIcon('id') }}
              </th>
              <th class="cursor-pointer" @click="toggleSort('note')">
                Note {{ sortIcon('note') }}
              </th>
              <th class="cursor-pointer" @click="toggleSort('tasks')">
                Tasks {{ sortIcon('tasks') }}
              </th>
              <th>ETA</th>
              <th class="cursor-pointer" @click="toggleSort('created_at')">
                Created {{ sortIcon('created_at') }}
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
              <td><StatusDot :status="j.status" kind="job" /></td>
              <td><code>{{ j.id.slice(0, 8) }}</code></td>
              <td class="text-on-surface-variant">{{ j.note || '—' }}</td>
              <td>
                <div class="d-flex align-center ga-2">
                  <v-progress-linear
                    :model-value="j.tasks.total > 0 ? (j.tasks.completed / j.tasks.total) * 100 : 0"
                    color="success" height="3" rounded style="width: 40px"
                  />
                  {{ j.tasks.completed }}/{{ j.tasks.total }}
                </div>
              </td>
              <td class="text-on-surface-variant">{{ j.eta_seconds ? formatDuration(j.eta_seconds) : '—' }}</td>
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
      <div class="d-flex align-center ga-2 px-4 py-3 cursor-pointer text-on-surface-variant" @click="archivedOpen = !archivedOpen">
        <v-icon size="16">mdi-archive-outline</v-icon>
        <span class="text-subtitle-2">{{ t('archive.section', { n: archivedJobs.length }) }}</span>
        <v-spacer />
        <v-icon size="16">{{ archivedOpen ? 'mdi-chevron-up' : 'mdi-chevron-down' }}</v-icon>
      </div>
      <div v-if="archivedOpen" class="px-2 pb-2">
        <div
          v-for="j in archivedJobs" :key="j.id"
          class="d-flex align-center ga-2 px-2 py-1 rounded recent-row cursor-pointer"
          @click="router.push({ name: 'job-detail', params: { project: j.project, jobId: j.id } })"
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
import type { JobSummary } from '@/types/api'

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
  } catch (e: any) { snack.error(e?.message || 'Archive failed') }
}

async function unarchiveJob(id: string) {
  try {
    await jobActions.unarchive.mutateAsync({ id, project: props.project })
    snack.success(t('archive.job_back'))
  } catch (e: any) { snack.error(e?.message || 'Unarchive failed') }
}

const statusFilter = ref('')
const searchQuery = ref('')
const sortKey = ref('created_at')
const sortDesc = ref(true)

// dot/kind reference statusGrammar — 'done' is a job-level status, the
// rest filter on task counts, hence task-level statuses.
const statusFilters: { value: string; label: string; dot: string; kind: 'task' | 'job' }[] = [
  { value: '', label: 'All', dot: '', kind: 'task' },
  { value: 'running', label: 'Running', dot: 'running', kind: 'task' },
  { value: 'done', label: 'Done', dot: 'done', kind: 'job' },
  { value: 'failed', label: 'Failed', dot: 'failed', kind: 'task' },
  { value: 'pending', label: 'Pending', dot: 'pending', kind: 'task' },
]

const projectJobs = computed(() =>
  projectArchived.value
    ? scopedJobs.value
    : (listQuery.data.value ?? []).filter(j => j.project === props.project))

const displayedJobs = computed(() => {
  let list = [...projectJobs.value]

  if (statusFilter.value) {
    list = list.filter(j => {
      if (statusFilter.value === 'running') return j.tasks.running > 0
      if (statusFilter.value === 'pending') return j.tasks.pending > 0
      if (statusFilter.value === 'failed') return j.tasks.failed > 0
      if (statusFilter.value === 'done') return j.status === 'done'
      return true
    })
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

function relativeTime(ts: number): string {
  const diff = Date.now() / 1000 - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

function formatDuration(sec: number): string {
  if (sec < 60) return `${sec}s`
  if (sec < 3600) return `${Math.floor(sec / 60)}m`
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`
}
</script>
