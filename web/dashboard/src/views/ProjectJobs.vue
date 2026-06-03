<template>
  <div>
    <!-- Header -->
    <div class="d-flex align-center justify-space-between mb-4">
      <div class="text-h5 font-weight-bold">{{ project }}</div>
      <v-btn variant="tonal" color="primary" size="small" :to="{ name: 'submit' }">
        <v-icon start size="16">mdi-plus</v-icon> {{ t('submit.new_job') }}
      </v-btn>
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
        <div v-if="s.dot" class="status-dot mr-1" :class="`status-dot--${s.dot}`" style="width: 6px; height: 6px" />
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
              @click="router.push({ name: 'job-detail', params: { project, jobId: j.id } })"
            >
              <td><div class="status-dot" :class="`status-dot--${j.status}`" /></td>
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
    <v-card v-else-if="!jobs.loading" class="pa-8 text-center">
      <v-icon size="36" color="primary" class="mb-3" style="opacity: 0.4">mdi-briefcase-outline</v-icon>
      <div class="text-h6 mb-1">{{ t('project.no_jobs') }}</div>
      <div class="text-body-2 text-on-surface-variant mb-4">{{ t('project.no_jobs_hint') }}</div>
      <v-btn color="primary" variant="tonal" :to="{ name: 'submit' }">{{ t('nav.submit') }}</v-btn>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useJobsStore } from '@/stores/jobs'

const props = defineProps<{ project: string }>()
const { t } = useI18n()
const jobs = useJobsStore()
const router = useRouter()

const statusFilter = ref('')
const searchQuery = ref('')
const sortKey = ref('created_at')
const sortDesc = ref(true)

const statusFilters = [
  { value: '', label: 'All', dot: '' },
  { value: 'running', label: 'Running', dot: 'running' },
  { value: 'done', label: 'Done', dot: 'completed' },
  { value: 'failed', label: 'Failed', dot: 'failed' },
  { value: 'pending', label: 'Pending', dot: 'pending' },
]

const projectJobs = computed(() => jobs.jobsByProject.get(props.project) ?? [])

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
