<template>
  <div>
    <!-- Header: the per-submit switch (skip covers ONE submit, never
         persisted) + the run trigger. Running is on demand — the probe
         executes real checks (SSH on remote targets), not per keystroke. -->
    <div class="d-flex align-center ga-2 px-4 py-2 border-b">
      <StatusDot :status="state.preflightEnabled ? 'success' : 'pending'" kind="task" :size="6" />
      <span class="text-caption font-weight-medium">{{ t('submit.preflight') }}</span>
      <v-chip
        size="x-small" :variant="state.preflightEnabled ? 'outlined' : 'tonal'"
        :color="state.preflightEnabled ? undefined : 'warning'"
        class="cursor-pointer"
        @click="state.preflightEnabled = !state.preflightEnabled"
      >
        {{ state.preflightEnabled ? t('submit.pf_skip_once') : t('submit.pf_skipped_once') }}
      </v-chip>
      <v-spacer />
      <v-btn size="x-small" variant="text" class="text-none" :loading="loading" @click="$emit('run')">
        <v-icon start size="13">mdi-play-outline</v-icon>{{ t('submit.pf_run_checks') }}
      </v-btn>
    </div>

    <!-- The contract: what this project PROMISES to verify on every submit.
         Facts from project.yaml — visible before any probe runs. -->
    <div class="d-flex align-center flex-wrap ga-1 px-4 py-2 text-caption text-on-surface-variant" :class="{ 'border-b': hasResults }">
      <span class="mr-1">{{ t('submit.pf_contract') }}:</span>
      <template v-if="contractEnabled">
        <v-chip size="x-small" variant="tonal" :color="contract.imports !== false ? 'success' : undefined">
          imports {{ contract.imports !== false ? '✓' : '—' }}
        </v-chip>
        <v-chip size="x-small" variant="tonal" :color="contract.wandb ? 'success' : undefined">
          wandb {{ contract.wandb ? '✓' : '—' }}
        </v-chip>
        <v-chip v-if="contract.extra_run" size="x-small" variant="tonal" color="success" :title="contract.extra_run">
          extra ✓
        </v-chip>
        <v-chip
          v-for="repo in contract.hf ?? []" :key="repo"
          size="x-small" variant="tonal" color="primary" class="font-mono" prepend-icon="mdi-cube-outline"
        >{{ repo }}</v-chip>
      </template>
      <span v-else>{{ t('submit.pf_contract_disabled') }}</span>
    </div>

    <!-- Results: the four-state grammar, problems first -->
    <div v-if="error" class="px-4 py-3 text-caption text-error">{{ error }}</div>
    <template v-else-if="report">
      <div
        v-for="c in orderedChecks" :key="c.name"
        class="d-flex align-start ga-2 px-4 py-2 check-row"
      >
        <v-icon size="15" :color="STATUS_ICON[c.status]?.color ?? 'on-surface-variant'" class="mt-1">
          {{ STATUS_ICON[c.status]?.icon ?? 'mdi-help-circle-outline' }}
        </v-icon>
        <div class="flex-grow-1 min-w-0">
          <span class="text-body-2 font-weight-medium font-mono">{{ c.name }}</span>
          <div v-if="c.detail" class="text-caption text-on-surface-variant" style="word-break: break-word">{{ c.detail }}</div>
          <div v-for="(cmd, i) in c.commands" :key="i" class="text-caption font-mono pl-2">$ {{ cmd }}</div>
          <!-- HF pre-download commands: the fix is one click away — setup
               runs once per submission on the login node, exactly where a
               `huggingface-cli download` belongs. -->
          <div v-if="c.commands?.length" class="d-flex align-center ga-2 mt-1">
            <v-btn
              size="x-small" variant="tonal" :disabled="setupAdded"
              :loading="setupSaving" @click="addToSetup(c.commands)"
            >
              <v-icon start size="12">mdi-playlist-plus</v-icon>
              {{ setupAdded ? t('submit.added_to_setup') : t('submit.add_to_setup') }}
            </v-btn>
            <span class="text-caption text-on-surface-variant">{{ t('submit.add_to_setup_hint') }}</span>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
// PreflightPanel (RQ2-3 c5, ex-RQ-80) — the preflight area as a
// contract-aware panel: what the project PROMISES (project.yaml's
// preflight block), a one-submit skip chip, and the probe's four-state
// results with the ready-made fix inline. Declaring scanned refs into
// preflight.hf was deliberately DROPPED (2026-08-19): blocking strength is
// identical for scanned refs, and the template form ({{param.NAME}}) — the
// one with real teeth — can't come from a button; it belongs to a future
// project-editor preflight section.
import { computed, inject, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import StatusDot from '@/components/StatusDot.vue'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import type { PreflightReport } from '@/types/api'
import { orderChecks } from './preflightPanel'
import { buildProjectPayload } from './submitFlow'
import { projectsApi } from '@/apis/projects'
import { useSnackbar } from '@/composables/useSnackbar'

const props = defineProps<{
  report: PreflightReport | null
  loading: boolean
  error: string
}>()
defineEmits<{ (e: 'run'): void }>()

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const snack = useSnackbar()

// CI-convention status → icon/color (statusGrammar vocabulary).
const STATUS_ICON: Record<string, { icon: string; color: string }> = {
  passed: { icon: 'mdi-check', color: 'success' },
  failed: { icon: 'mdi-alert-circle', color: 'error' },
  warning: { icon: 'mdi-alert-circle-outline', color: 'warning' },
  skipped: { icon: 'mdi-minus-circle-outline', color: 'neutral' },
}

const contract = computed(() => state.newProject.source?.preflight ?? {})
const contractEnabled = computed(() => contract.value.enabled !== false)
const orderedChecks = computed(() => orderChecks(props.report?.results))
const hasResults = computed(() => !!props.report || !!props.error)

// ── Add to setup (semantics unchanged from StepReview/LivePlan) ──
const setupSaving = ref(false)
const setupAdded = ref(false)

// A fresh report means a fresh remediation state: an "Added" badge from the
// previous round next to this round's commands could mislead.
watch(() => props.report, () => {
  setupAdded.value = false
  setupSaving.value = false
})

async function addToSetup(cmds: string[]) {
  const fresh = cmds.filter(c => !state.newProject.setupCmd.includes(c))
  if (fresh.length === 0) {
    setupAdded.value = true
    return
  }
  setupSaving.value = true
  try {
    const prev = state.newProject.setupCmd.trim()
    state.newProject.setupCmd = [prev, ...fresh].filter(Boolean).join('\n')
    const name = state.projectName || state.newProject.name.trim()
    await projectsApi.update(name, buildProjectPayload(state.newProject, state.newProject.source))
    setupAdded.value = true
    snack.success(t('submit.added_to_setup_done', { name }))
  } catch (e: any) {
    snack.error(e?.message || t('common.error'))
  } finally {
    setupSaving.value = false
  }
}
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.min-w-0 { min-width: 0; }
.border-b { border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.check-row + .check-row { border-top: 0.5px solid rgb(var(--v-theme-outline-variant), 0.5); }
</style>
