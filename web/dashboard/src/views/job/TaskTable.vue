<template>
  <v-card class="pa-0">
    <div class="overflow-x-auto">
      <table class="data-mono" style="width: 100%">
        <thead>
          <tr>
            <th style="width:24px"></th>
            <th>ID</th>
            <th>Params</th>
            <th>Step</th>
            <th>Elapsed</th>
            <th v-if="hasWandb">W&B</th>
            <th style="width:70px"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="task in tasks" :key="task.id">
            <td><div class="status-dot" :class="`status-dot--${task.status}`" /></td>
            <td><code>{{ task.id.slice(0, 8) }}</code></td>
            <td class="text-on-surface-variant">{{ compactParams(task.params) }}</td>
            <td>{{ task.current_step ?? '-' }}</td>
            <td class="text-on-surface-variant">{{ task.elapsed_seconds ? formatDuration(task.elapsed_seconds) : '-' }}</td>
            <td v-if="hasWandb">
              <v-btn v-if="task.wandb_run_id" size="x-small" variant="text" icon
                :href="wandbRunURL(task.wandb_run_id)" target="_blank"
              ><v-icon size="14">mdi-open-in-new</v-icon></v-btn>
            </td>
            <td>
              <div class="d-flex ga-1">
                <v-btn v-if="task.status === 'running'" icon size="x-small" variant="text" color="error"
                  @click="$emit('kill-task', task.id)"
                ><v-icon size="14">mdi-stop</v-icon></v-btn>
                <v-btn v-if="task.status === 'failed'" icon size="x-small" variant="text" color="primary"
                  @click="$emit('retry-task', task.id)"
                ><v-icon size="14">mdi-refresh</v-icon></v-btn>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-if="tasks.length === 0" class="text-center text-on-surface-variant pa-6">
      No tasks match the filter
    </div>
  </v-card>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { TaskView, WandbInfo } from '@/types/api'

const props = defineProps<{
  tasks: TaskView[]
  wandb?: WandbInfo | null
}>()

defineEmits<{ 'kill-task': [id: string]; 'retry-task': [id: string] }>()

const hasWandb = computed(() => !!props.wandb)

function wandbRunURL(runId: string): string {
  if (props.wandb?.base_url) return `${props.wandb.base_url}/runs/${runId}`
  return `https://wandb.ai/runs/${runId}`
}

function compactParams(params: Record<string, any>): string {
  const entries = Object.entries(params)
  if (entries.length === 0) return '{}'
  const parts = entries.slice(0, 3).map(([k, v]) => `${k}=${v}`)
  if (entries.length > 3) parts.push('...')
  return parts.join(', ')
}

function formatDuration(sec: number): string {
  const s = Math.round(sec)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
</script>
