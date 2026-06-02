<template>
  <div>
    <!-- Breadcrumb -->
    <div class="d-flex align-center ga-1 mb-4 text-caption text-on-surface-variant">
      <router-link :to="{ name: 'overview' }" class="text-decoration-none text-on-surface-variant">
        {{ t('nav.overview') }}
      </router-link>
      <v-icon size="14">mdi-chevron-right</v-icon>
      <span class="text-on-surface font-weight-medium">{{ project }}</span>
    </div>

    <!-- Header -->
    <div class="d-flex align-center justify-space-between mb-4">
      <div class="text-h5 font-weight-bold">{{ project }}</div>
      <v-btn
        variant="tonal"
        color="primary"
        size="small"
        :to="{ name: 'submit' }"
      >
        <v-icon start size="16">mdi-plus</v-icon>
        {{ t('submit.new_job') }}
      </v-btn>
    </div>

    <!-- Jobs list -->
    <div v-if="projectJobs.length > 0" class="d-flex flex-column ga-3 card-stagger">
      <v-card
        v-for="j in projectJobs"
        :key="j.id"
        class="pa-4 cursor-pointer"
        hover
        @click="router.push({ name: 'job-detail', params: { project, jobId: j.id } })"
      >
        <div class="d-flex align-center ga-3">
          <StatusBadge :status="j.status" />
          <div class="flex-grow-1">
            <div class="d-flex align-center ga-2">
              <code class="text-body-2">{{ j.id.slice(0, 8) }}</code>
              <span v-if="j.note" class="text-caption text-on-surface-variant">{{ j.note }}</span>
            </div>
          </div>
          <div class="d-flex align-center ga-3">
            <div class="d-flex align-center ga-2" style="width: 120px">
              <v-progress-linear
                :model-value="j.tasks.total > 0 ? (j.tasks.completed / j.tasks.total) * 100 : 0"
                color="success"
                height="4"
                rounded
                class="flex-grow-1"
              />
              <span class="text-caption text-on-surface-variant text-no-wrap">
                {{ j.tasks.completed }}/{{ j.tasks.total }}
              </span>
            </div>
            <span v-if="j.eta_seconds" class="text-caption text-on-surface-variant text-no-wrap">
              {{ formatDuration(j.eta_seconds) }}
            </span>
            <span class="text-caption text-on-surface-variant text-no-wrap" style="min-width: 60px; text-align: right">
              {{ relativeTime(j.created_at) }}
            </span>
            <v-icon size="16" color="on-surface-variant">mdi-chevron-right</v-icon>
          </div>
        </div>
      </v-card>
    </div>

    <!-- Empty state -->
    <v-card v-else-if="!jobs.loading" class="pa-8 text-center">
      <v-icon size="40" color="primary" class="mb-3" style="opacity: 0.4">mdi-briefcase-outline</v-icon>
      <div class="text-h6 mb-1">{{ t('project.no_jobs') }}</div>
      <div class="text-body-2 text-on-surface-variant mb-4">{{ t('project.no_jobs_hint') }}</div>
      <v-btn color="primary" variant="tonal" :to="{ name: 'submit' }">
        {{ t('nav.submit') }}
      </v-btn>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useJobsStore } from '@/stores/jobs'
import StatusBadge from '@/components/StatusBadge.vue'

const props = defineProps<{ project: string }>()
const { t } = useI18n()
const jobs = useJobsStore()
const router = useRouter()

const projectJobs = computed(() => jobs.jobsByProject.get(props.project) ?? [])

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
