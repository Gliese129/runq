<template>
  <div class="plan-panel">
    <v-card class="pa-0">
      <!-- Count + formula: sweep shaping is a loop, the plan answers live -->
      <div class="px-4 py-3 border-b">
        <div class="text-caption text-on-surface-variant">{{ t('submit.this_submit') }}</div>
        <div class="d-flex align-baseline ga-2">
          <span class="text-h4 font-weight-medium" :class="{ 'text-error': total === 0 }">{{ total }}</span>
          <span class="text-body-2 text-on-surface-variant">{{ t('submit.tasks_word', total) }}</span>
          <v-progress-circular v-if="state.dryRunLoading" indeterminate size="12" width="2" color="primary" />
        </div>
        <code class="text-caption text-on-surface-variant" style="word-break: break-word">
          {{ state.sweepSummary || t('submit.no_sweep_hint') }} · {{ t('submit.n_fixed', { n: fixedCount }) }}
        </code>
        <div v-if="resolvedNote" class="text-caption text-on-surface-variant text-truncate font-mono mt-1" :title="resolvedNote">
          → {{ resolvedNote }}
        </div>
      </div>

      <!-- First combinations -->
      <div v-if="state.dryRunError" class="pa-4 text-caption text-error">{{ state.dryRunError }}</div>
      <div v-else-if="total > 0" class="plan-table overflow-y-auto">
        <table class="data-mono w-100">
          <thead>
            <tr>
              <th>#</th>
              <th v-for="k in cols" :key="k" class="text-primary">{{ k }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, i) in state.dryRunResult.slice(0, 24)" :key="i">
              <td class="text-on-surface-variant">{{ i + 1 }}</td>
              <td v-for="k in cols" :key="k" class="text-primary">{{ row[k] }}</td>
            </tr>
          </tbody>
        </table>
        <div v-if="total > 24" class="px-3 py-2 text-caption text-on-surface-variant font-mono">
          … {{ t('submit.n_more', { n: total - 24 }) }}
        </div>
      </div>
      <div v-else class="pa-4 text-body-2 text-on-surface-variant">
        {{ t('submit.no_tasks_hint') }}
      </div>

      <!-- Preflight: contract + one-submit skip + on-demand results
           (RQ2-3 c5). Shares this panel's previewSubmit fetch. -->
      <div class="border-t">
        <PreflightPanel
          :report="previewPreflight"
          :loading="previewLoading"
          :error="previewError"
          @run="fetchPreview"
        />
      </div>
    </v-card>

    <!-- Rendered submit preview — the GUI face of `--dry-run` (capability
         submit_preview). Fetch on expand: it runs the real submit code
         path incl. preflight probes, too heavy per keystroke. -->
    <v-card v-if="config.caps.submit_preview" class="pa-0 mt-3">
      <div
        class="d-flex align-center ga-2 px-4 py-3 cursor-pointer"
        role="button" tabindex="0" :aria-expanded="previewOpen"
        @click="togglePreview"
        @keydown.enter="togglePreview"
        @keydown.space.prevent="togglePreview"
      >
        <v-icon size="16" color="primary">mdi-console-line</v-icon>
        <span class="text-subtitle-2">{{ t('submit.rendered_cmds', { n: total }) }}</span>
        <v-spacer />
        <v-progress-circular v-if="previewLoading" indeterminate size="14" width="2" color="primary" />
        <v-icon size="16">{{ previewOpen ? 'mdi-chevron-up' : 'mdi-chevron-down' }}</v-icon>
      </div>
      <div v-if="previewOpen" class="px-4 pb-4">
        <div v-if="previewError" class="text-caption text-error">{{ previewError }}</div>
        <pre v-else-if="previewText" class="preview-pre rounded pa-3 text-body-2">{{ previewText }}</pre>
        <div v-else-if="!previewLoading" class="text-caption text-on-surface-variant">{{ t('submit.no_preview') }}</div>
      </div>
    </v-card>

  </div>
</template>

<script setup lang="ts">
// LivePlan (RQ2-3 c2/c5, kit ScreensSubmit) — the Review step as a
// persistent panel: sweep shaping is a loop, not a phase. Counts and the
// first combinations answer while the user edits; PreflightPanel (contract
// + on-demand checks) and the capability-gated rendered preview share one
// previewSubmit fetch below.
import { computed, inject, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import PreflightPanel from './PreflightPanel.vue'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import type { PreflightReport } from '@/types/api'
import { sweptParamNames as computeSwept, fixedParamPreview, buildJobConfig } from './submitFlow'
import { jobsApi } from '@/apis/jobs'
import { useConfigStore } from '@/stores/config'

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const config = useConfigStore()

const total = computed(() => state.displayTaskCount)
const swept = computed(() => computeSwept(state.rows, state.linkSets))
const fixedCount = computed(() => Object.keys(fixedParamPreview(state.rows, state.linkSets)).length)

const cols = computed(() => {
  const keys = new Set<string>()
  for (const row of state.dryRunResult) for (const k of Object.keys(row)) keys.add(k)
  return [...keys].filter(k => swept.value.has(k))
})

const resolvedNote = computed(() =>
  state.note.includes('{{') && state.noteResolved ? state.noteResolved : '')

// ── Rendered preview (fetch on expand — it runs preflight probes) ──
const previewOpen = ref(false)
const previewText = ref('')
const previewPreflight = ref<PreflightReport | null>(null)
const previewLoading = ref(false)
const previewError = ref('')
function togglePreview() {
  previewOpen.value = !previewOpen.value
  if (previewOpen.value) void fetchPreview()
}

// The plan changed under an open preview → the shown render is stale
// truth. Close and clear; the next expand re-fetches (U1).
watch(() => state.dryRunResult, () => {
  previewOpen.value = false
  previewText.value = ''
  previewPreflight.value = null
  previewError.value = ''
})

async function fetchPreview() {
  previewLoading.value = true
  previewError.value = ''
  previewText.value = ''
  previewPreflight.value = null
  try {
    const cfg = buildJobConfig(state.projectName, state.note, state.rows, state.linkSets)
    if (state.jobName.trim()) cfg.name = state.jobName.trim()
    const res = await jobsApi.previewSubmit(cfg, !state.preflightEnabled, state.target)
    previewText.value = res.preview
    previewPreflight.value = res.preflight ?? null
  } catch (e: any) {
    previewError.value = e?.message || t('common.error')
  } finally {
    previewLoading.value = false
  }
}

</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.w-100 { width: 100%; }
.border-b { border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.plan-panel { position: sticky; top: 72px; }
@media (max-width: 959px) {
  .plan-panel { position: static; }
}
.plan-table { max-height: 340px; }
.border-t { border-top: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.preview-pre {
  background: rgb(var(--v-theme-surface-variant), 0.4);
  overflow-x: auto;
  white-space: pre;
  font-family: var(--font-mono);
  font-size: 12px;
  line-height: 1.5;
  max-height: 420px;
  overflow-y: auto;
}
</style>
