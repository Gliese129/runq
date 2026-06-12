<template>
  <v-card class="pa-0">
    <div class="overflow-x-auto">
      <table class="data-mono" style="width: 100%">
        <thead>
          <tr>
            <th style="width:50px">#</th>
            <th>Task</th>
            <th>Params</th>
            <th>{{ metricKey }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in rows" :key="row.task_id">
            <td>
              <div class="d-flex align-center ga-1">
                <v-icon v-if="row.rank === 1" size="14" color="warning">mdi-trophy</v-icon>
                <v-icon v-else-if="row.rank <= 3" size="14" color="on-surface-variant">mdi-medal</v-icon>
                <span :class="{ 'font-weight-medium': row.rank <= 3 }">{{ row.rank }}</span>
              </div>
            </td>
            <td><code>{{ row.task_id.slice(0, 8) }}</code></td>
            <td class="text-on-surface-variant">{{ compactParams(row.params) }}</td>
            <td class="font-weight-medium" :class="row.rank === 1 ? 'text-success' : ''">
              {{ typeof row.best === 'number' ? row.best.toPrecision(4) : '-' }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-if="rows.length === 0" class="text-center text-on-surface-variant pa-6">
      No comparison data
    </div>
  </v-card>
</template>

<script setup lang="ts">
import type { CompareRow } from '@/types/api'

defineProps<{
  rows: CompareRow[]
  metricKey: string
}>()

function compactParams(params: Record<string, any>): string {
  const entries = Object.entries(params)
  if (entries.length === 0) return '{}'
  const parts = entries.slice(0, 3).map(([k, v]) => `${k}=${v}`)
  if (entries.length > 3) parts.push('...')
  return parts.join(', ')
}
</script>
