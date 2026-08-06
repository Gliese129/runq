<template>
  <div style="max-width: 960px; margin: 0 auto">
    <v-row class="mb-4" dense>
      <v-col cols="12" sm="3">
        <v-card class="pa-3 text-center">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.total_tasks') }}</div>
          <div class="text-h5 font-weight-medium">{{ state.dryRunResult.length }}</div>
        </v-card>
      </v-col>
      <v-col cols="12" sm="3">
        <v-card class="pa-3 text-center">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.sweep_label') }}</div>
          <div class="text-body-2 font-weight-medium" style="word-break: break-word">{{ state.sweepSummary || '-' }}</div>
        </v-card>
      </v-col>
      <v-col cols="12" sm="3">
        <v-card class="pa-3 text-center">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.project') }}</div>
          <div class="text-body-2 font-weight-medium text-truncate">{{ state.projectName }}</div>
          <div v-if="resolvedNote" class="text-caption text-on-surface-variant text-truncate" style="font-family: monospace" :title="resolvedNote">
            {{ resolvedNote }}
          </div>
          <div class="text-caption text-on-surface-variant text-truncate" style="font-family: monospace" :title="t('submit.job_name_render_hint')">
            <v-icon size="10">mdi-tag-outline</v-icon> {{ effectiveJobName }}
          </div>
        </v-card>
      </v-col>
      <v-col cols="12" sm="3">
        <v-card class="pa-3">
          <div class="d-flex align-center justify-space-between ga-3">
            <div class="flex-grow-1">
              <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.preflight') }}</div>
              <div
                class="text-body-2 font-weight-medium"
                :class="state.preflightEnabled ? 'text-success' : 'text-warning'"
              >
                {{ state.preflightEnabled ? t('common.on') : t('common.off') }}
              </div>
            </div>
            <v-switch
              v-model="state.preflightEnabled"
              :color="state.preflightEnabled ? 'success' : 'warning'"
              density="compact"
              hide-details
              inset
              :aria-label="t('submit.preflight')"
            />
          </div>
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
            {{ t('submit.swept_fixed', { s: sweptParamNames.size, f: fixedParamCount }) }}
          </div>
        </div>
        <v-btn v-if="state.dryRunResult.length > 0" size="x-small" variant="text" color="primary" @click="state.step--">
          <v-icon start size="14">mdi-pencil-outline</v-icon> {{ t('common.edit') }}
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
          <v-icon start size="14">mdi-arrow-left</v-icon> {{ t('submit.back_to_configure') }}
        </v-btn>
      </div>
    </v-card>

    <!-- Preflight suggestions with ready-made commands (HF pre-download):
         warnings never block, but the fix is one click away — append the
         commands to the project's setup command (runs on the login node
         before submission, where internet is available). -->
    <v-card
      v-for="c in previewSuggestions" :key="c.name"
      variant="tonal" :color="c.status === 'failed' ? 'error' : 'warning'" class="mt-3 pa-3"
    >
      <div class="d-flex align-center ga-2 mb-1">
        <v-icon size="16">mdi-cloud-download-outline</v-icon>
        <span class="text-body-2 font-weight-medium">{{ t('submit.preflight_suggestion', { name: c.name }) }}</span>
      </div>
      <div class="text-caption mb-2" style="word-break: break-word">{{ c.detail }}</div>
      <div v-for="(cmd, i) in c.commands" :key="i" class="text-caption pl-2" style="font-family: monospace">$ {{ cmd }}</div>
      <div class="d-flex align-center ga-2 mt-2">
        <v-btn
          size="x-small" variant="tonal" :disabled="setupCmdAdded"
          :loading="setupCmdSaving" @click="appendToSetupCommand(c.commands || [])"
        >
          <v-icon start size="12">mdi-playlist-plus</v-icon>
          {{ setupCmdAdded ? t('submit.added_to_setup') : t('submit.add_to_setup') }}
        </v-btn>
        <span class="text-caption text-on-surface-variant">{{ t('submit.add_to_setup_hint') }}</span>
      </div>
    </v-card>

    <!-- Rendered submit preview — the GUI face of `--dry-run`: same backend
         code path, zero side effects. Only rendered when the backend declares
         the capability (U2: shape from capabilities). -->
    <v-card v-if="config.caps.submit_preview && state.dryRunResult.length > 0" class="mt-3">
      <div
        class="d-flex align-center ga-2 px-4 py-3 cursor-pointer"
        role="button" tabindex="0" :aria-expanded="previewOpen"
        @click="previewOpen = !previewOpen"
        @keydown.enter="previewOpen = !previewOpen"
        @keydown.space.prevent="previewOpen = !previewOpen"
      >
        <v-icon size="16" color="primary">mdi-console-line</v-icon>
        <span class="text-subtitle-2">{{ t('submit.rendered_cmds', { n: state.dryRunResult.length }) }}</span>
        <span class="text-caption text-on-surface-variant">{{ t('submit.rendered_sub') }}</span>
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
import { computed, inject, onActivated, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import type { PreflightReport } from '@/types/api'
import { sweptParamNames as computeSwept, fixedParamPreview, buildJobConfig, buildProjectPayload } from './submitFlow'
import { jobsApi } from '@/apis/jobs'
import { projectsApi } from '@/apis/projects'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfigStore } from '@/stores/config'

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const config = useConfigStore()
const snack = useSnackbar()

// Rendered submit preview (run.sh + submit command for the first task) —
// fetched from the submit code path, never simulated here (U1).
const previewOpen = ref(true)
const previewText = ref('')
const previewPreflight = ref<PreflightReport | null>(null)
const previewLoading = ref(false)
const previewError = ref('')
const setupCmdSaving = ref(false)
const setupCmdAdded = ref(false)
// onActivated (not onMounted): this component lives inside <KeepAlive>,
// so it must re-fetch every time the user re-enters review — a cached
// preview of edited params would violate preview-is-backend-truth (U1).
// onActivated also fires on the first mount.
onActivated(async () => {
  if (!config.caps.submit_preview) return
  previewLoading.value = true
  previewError.value = ''
  // Every fetch starts from a clean slate (Codex r1 #6): stale HF
  // suggestions / an "Added" badge from the PREVIOUS report next to this
  // round's error can push a wrong command into the project.
  previewText.value = ''
  previewPreflight.value = null
  setupCmdAdded.value = false
  setupCmdSaving.value = false
  try {
    const cfg = buildJobConfig(state.projectName, state.note, state.rows, state.linkSets)
    if (state.jobName.trim()) cfg.name = state.jobName.trim()
    const res = await jobsApi.previewSubmit(cfg, !state.preflightEnabled)
    previewText.value = res.preview
    previewPreflight.value = res.preflight ?? null
  } catch (e: any) {
    previewError.value = e?.message || t('common.error')
  } finally {
    previewLoading.value = false
  }
})

// Final job name as it WILL be submitted ({{version}} already scanned) —
// resolved by the backend via POST /jobs/plan (runs on every entry into
// review), never simulated here (U1).
const resolvedNote = computed(() =>
  state.note.includes('{{') ? state.noteResolved : state.note)

// Effective scheduler job name TEMPLATE (override > project > default).
// The rendered per-task value is backend truth — visible in the Rendered
// commands block below, never simulated here (U1).
const effectiveJobName = computed(() =>
  state.jobName.trim() || state.newProject.jobName || 'rq-{{task_id\u007d\u007d')

// Preflight results that carry ready-made remediation commands (today:
// uncached HF repos). Rendered as actionable cards, not buried in text.
const previewSuggestions = computed(() =>
  (previewPreflight.value?.results ?? []).filter(c => c.commands?.length))

/** Append the suggested commands to the project's setup command and save
 *  the project in place — setup runs once per submission on the login
 *  node, exactly where a `huggingface-cli download` belongs. */
async function appendToSetupCommand(cmds: string[]) {
  const fresh = cmds.filter(c => !state.newProject.setupCmd.includes(c))
  if (fresh.length === 0) {
    setupCmdAdded.value = true
    return
  }
  setupCmdSaving.value = true
  try {
    const prev = state.newProject.setupCmd.trim()
    state.newProject.setupCmd = [prev, ...fresh].filter(Boolean).join('\n')
    const name = state.projectName || state.newProject.name.trim()
    await projectsApi.update(name, buildProjectPayload(state.newProject, state.newProject.source))
    setupCmdAdded.value = true
    snack.success(t('submit.added_to_setup_done', { name }))
  } catch (e: any) {
    snack.error(e?.message || t('common.error'))
  } finally {
    setupCmdSaving.value = false
  }
}

const sweptParamNames = computed(() => computeSwept(state.rows, state.linkSets))
const fixedParams = computed(() => fixedParamPreview(state.rows, state.linkSets))
const fixedParamCount = computed(() => Object.keys(fixedParams.value).length)

function columnClass(key: string): string {
  if (sweptParamNames.value.has(key)) return 'swept-col'
  if (key in fixedParams.value) return 'fixed-col'
  return ''
}
</script>

<style scoped>
.preview-pre {
  background: rgb(var(--v-theme-surface-variant), 0.4);
  overflow-x: auto;
  white-space: pre;
  font-family: ui-monospace, monospace;
  font-size: 12px;
  line-height: 1.5;
  max-height: 420px;
  overflow-y: auto;
}

.swept-col { color: rgb(var(--v-theme-primary)); font-weight: 500; }
.fixed-col { color: rgb(var(--v-theme-on-surface-variant)); }
</style>
