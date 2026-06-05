<template>
  <div style="max-width: 960px; margin: 0 auto">
    <v-row class="mb-4" dense>
      <v-col cols="12" sm="4">
        <v-card class="pa-3 text-center">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.total_tasks') }}</div>
          <div class="text-h5 font-weight-medium">{{ state.dryRunResult.length }}</div>
        </v-card>
      </v-col>
      <v-col cols="12" sm="4">
        <v-card class="pa-3 text-center">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.sweep_label') }}</div>
          <div class="text-body-2 font-weight-medium" style="word-break: break-word">{{ state.sweepSummary || '-' }}</div>
        </v-card>
      </v-col>
      <v-col cols="12" sm="4">
        <v-card class="pa-3 text-center">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.project') }}</div>
          <div class="text-body-2 font-weight-medium text-truncate">{{ state.projectName }}</div>
        </v-card>
      </v-col>
    </v-row>

    <!-- Fixed params summary (if any) -->
    <div v-if="fixedParamCount > 0" class="d-flex flex-wrap ga-2 mb-3">
      <v-chip v-for="(val, key) in fixedParams" :key="key" size="small" variant="tonal" label>
        <span class="font-weight-medium">{{ key }}</span>
        <span class="text-on-surface-variant ml-1">= {{ val }}</span>
      </v-chip>
    </div>

    <v-card class="pa-4">
      <div class="d-flex align-center justify-space-between mb-3">
        <div class="d-flex align-center ga-2">
          <div class="text-subtitle-2">{{ t('submit.preview') }}</div>
          <div v-if="fixedParamCount > 0" class="text-caption text-on-surface-variant">
            {{ sweptParamNames.size }} swept · {{ fixedParamCount }} fixed
          </div>
        </div>
        <v-btn v-if="state.dryRunResult.length > 0" size="x-small" variant="text" color="primary" @click="state.step--">
          <v-icon start size="14">mdi-pencil-outline</v-icon> Edit
        </v-btn>
      </div>

      <div v-if="state.dryRunLoading" class="d-flex justify-center pa-8">
        <v-progress-circular indeterminate color="primary" />
      </div>

      <div v-else-if="state.dryRunResult.length > 0" class="overflow-x-auto">
        <table class="data-mono" style="width: 100%">
          <thead>
            <tr>
              <th>#</th>
              <th
                v-for="h in state.dryRunHeaders" :key="h.key"
                :class="columnClass(h.key)"
              >{{ h.title }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, i) in state.dryRunResult" :key="i">
              <td class="text-on-surface-variant">{{ i + 1 }}</td>
              <td
                v-for="h in state.dryRunHeaders" :key="h.key"
                :class="columnClass(h.key)"
              >{{ row[h.key] }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-else class="text-center text-on-surface-variant pa-8">
        <v-icon size="28" class="mb-2">mdi-alert-circle-outline</v-icon>
        <div class="text-body-2">{{ state.dryRunError || t('submit.no_tasks') }}</div>
        <div v-if="!state.dryRunError" class="text-caption mt-1">{{ t('submit.no_tasks_hint') }}</div>
        <v-btn size="small" variant="tonal" color="primary" class="mt-3" @click="state.step--">
          <v-icon start size="14">mdi-arrow-left</v-icon> Back to configure
        </v-btn>
      </div>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { computed, inject } from 'vue'
import { useI18n } from 'vue-i18n'
import { SUBMIT_STATE_KEY, type SweepGroup, type GroupParam } from '@/types/submit'

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!

// Swept params: in a sweep group with >1 value
const sweptParamNames = computed(() => {
  const names = new Set<string>()
  for (const g of state.groups as SweepGroup[]) {
    for (const p of g.params as GroupParam[]) {
      if (p.values.filter(v => !isBlankValue(v)).length > 0) names.add(p.name)
    }
  }
  return names
})

// Fixed params: included, have default, not swept
const fixedParams = computed(() => {
  const fp: Record<string, any> = {}
  for (const p of state.newProject.params) {
    if (!p.include || isBlankValue(p.default) || sweptParamNames.value.has(p.name)) continue
    fp[p.name] = p.default
  }
  return fp
})

const fixedParamCount = computed(() => Object.keys(fixedParams.value).length)

function columnClass(key: string): string {
  if (sweptParamNames.value.has(key)) return 'swept-col'
  if (key in fixedParams.value) return 'fixed-col'
  return ''
}

function isBlankValue(v: string): boolean {
  return String(v ?? '').trim() === ''
}
</script>

<style scoped>
.swept-col { color: rgb(var(--v-theme-primary)); font-weight: 500; }
.fixed-col { color: rgb(var(--v-theme-on-surface-variant)); }
</style>
