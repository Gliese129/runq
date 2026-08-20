<template>
  <v-card class="pa-0 mb-3">
    <div class="d-flex align-center ga-2 px-4 py-2 flex-wrap">
      <!-- Project pill -->
      <v-menu location="bottom start">
        <template #activator="{ props: menuProps }">
          <button type="button" class="pill d-flex align-center ga-2" v-bind="menuProps">
            <v-icon size="15" :color="state.projectName ? 'primary' : 'on-surface-variant'">mdi-folder-outline</v-icon>
            <span class="text-body-2 font-weight-medium" :class="{ 'text-on-surface-variant': !state.projectName }">
              {{ state.projectName || t('submit.choose_project') }}
            </span>
            <v-icon size="13" color="on-surface-variant">mdi-chevron-down</v-icon>
          </button>
        </template>
        <v-list density="compact" min-width="240">
          <v-list-item
            v-for="p in sortedProjects" :key="p.name"
            :active="p.name === state.projectName"
            color="primary"
            @click="$emit('select-project', p.name)"
          >
            <v-list-item-title class="text-body-2">{{ p.name }}</v-list-item-title>
            <template #append>
              <span class="text-caption text-on-surface-variant font-mono">{{ t('project.job_count', { n: p.job_count }, p.job_count) }}</span>
            </template>
          </v-list-item>
          <v-divider class="my-1" />
          <v-list-item @click="router.push({ name: 'project-new', query: { redirect: 'submit' } })">
            <v-list-item-title class="text-body-2 text-primary">+ {{ t('submit.new_project') }}…</v-list-item-title>
          </v-list-item>
          <v-list-item
            v-if="state.projectName"
            @click="router.push({ name: 'project-edit', params: { project: state.projectName }, query: { redirect: 'submit' } })"
          >
            <v-list-item-title class="text-body-2">{{ t('projectEdit.edit_project_named', { name: state.projectName }) }}…</v-list-item-title>
          </v-list-item>
        </v-list>
      </v-menu>

      <v-icon size="14" color="on-surface-variant">mdi-arrow-right</v-icon>

      <!-- Target pill: available / not set up ONLY (registry fact — the
           project config pins its target). No drift / sync states: the
           placement model is Phase 2. -->
      <v-menu location="bottom start">
        <template #activator="{ props: menuProps }">
          <button type="button" class="pill d-flex align-center ga-2" :class="{ 'pill--error': notSetUp }" v-bind="menuProps">
            <v-icon size="15" color="on-surface-variant">mdi-server</v-icon>
            <span class="text-body-2 font-weight-medium">{{ state.target }}</span>
            <v-icon size="13" color="on-surface-variant">mdi-chevron-down</v-icon>
          </button>
        </template>
        <v-list density="compact" min-width="240">
          <v-list-item
            v-for="tgt in config.targets" :key="tgt.name"
            :active="tgt.name === state.target"
            color="primary"
            @click="$emit('select-target', tgt.name)"
          >
            <v-list-item-title class="text-body-2">{{ tgt.name }}</v-list-item-title>
            <v-list-item-subtitle class="text-caption">{{ tgt.type === 'remote' ? tgt.scheduler || 'remote' : 'daemon' }}</v-list-item-subtitle>
            <template #append>
              <span class="text-caption text-on-surface-variant">{{ availabilityLabel(tgt.name) }}</span>
            </template>
          </v-list-item>
        </v-list>
      </v-menu>

      <v-spacer />

      <v-btn v-if="hasState" size="x-small" variant="text" :title="t('submit.reset_title')" @click="$emit('reset')">
        <v-icon start size="12">mdi-restore</v-icon> {{ t('submit.reset') }}
      </v-btn>
      <v-btn size="x-small" variant="text" @click="$emit('import-yaml')">
        <v-icon start size="12">mdi-file-import-outline</v-icon> {{ t('submit.import_yaml') }}
      </v-btn>
      <v-btn
        color="primary" variant="flat"
        :loading="state.submitting"
        :disabled="!state.projectName || notSetUp || total === 0 || !valid"
        @click="$emit('submit')"
      >
        <v-icon start size="16">mdi-rocket-launch-outline</v-icon>
        {{ total > 0 ? t('submit.submit_n', { n: total }, total) : t('submit.submit') }}
      </v-btn>
    </div>

    <!-- Placement truth at the moment it matters — before the run. The
         project's configured target is the registry fact; submitting
         elsewhere would run against a working_dir that isn't there. -->
    <div v-if="notSetUp" class="d-flex align-center ga-2 px-4 py-2 notsetup-band">
      <v-icon size="14" color="error">mdi-alert-circle-outline</v-icon>
      <span class="text-body-2">{{ t('submit.not_set_up', { project: state.projectName, target: state.target }) }}</span>
      <span class="text-caption text-on-surface-variant">
        {{ t('submit.not_set_up_hint', { target: projectTarget }) }}
      </span>
    </div>
  </v-card>
</template>

<script setup lang="ts">
// IdentityBar (RQ2-3 c2, kit ScreensSubmit) — project + target live in a
// persistent bar instead of a wizard step; the CTA carries the task count.
// Placement is DOWNGRADED by ruling: available / not-set-up only, no
// Deploy/Sync actions (Phase 2 owns the placement model).
import { computed, inject } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useConfigStore } from '@/stores/config'
import { usePreferences } from '@/composables/usePreferences'
import { SUBMIT_STATE_KEY } from '@/types/submit'

defineProps<{ total: number; valid: boolean; hasState: boolean }>()
defineEmits<{
  (e: 'select-project', name: string): void
  (e: 'select-target', name: string): void
  (e: 'submit'): void
  (e: 'import-yaml'): void
  (e: 'reset'): void
}>()

const { t } = useI18n()
const router = useRouter()
const config = useConfigStore()
const prefs = usePreferences()
const state = inject(SUBMIT_STATE_KEY)!

const sortedProjects = computed(() => {
  const list = state.matchedProjects.filter(p => !p.archived)
  return [...list].sort((a, b) => {
    if (a.name === prefs.lastProject.value) return -1
    if (b.name === prefs.lastProject.value) return 1
    return b.job_count - a.job_count
  })
})

// The registry fact: a project's config pins the target it submits to.
// Empty = never pinned (pre-multi-target configs) → treat as at home on
// the default target, not as an error.
const projectTarget = computed(() =>
  state.newProject.source?.target || config.currentTarget)

const notSetUp = computed(() =>
  !!state.projectName && !!state.target && state.target !== projectTarget.value)

function availabilityLabel(target: string): string {
  if (!state.projectName) return ''
  return target === projectTarget.value ? t('submit.target_available') : t('submit.target_not_set_up')
}
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.pill {
  padding: 5px 10px;
  border-radius: var(--radius-lg);
  border: 1px solid rgb(var(--v-theme-outline-variant));
  background: rgb(var(--v-theme-surface));
  cursor: pointer;
  white-space: nowrap;
  transition: var(--transition);
}
.pill:hover { border-color: rgb(var(--v-theme-primary)); }
.pill--error { border-color: rgb(var(--v-theme-error), 0.55); }
.notsetup-band {
  border-top: 0.5px solid rgb(var(--v-theme-outline-variant));
  background: rgb(var(--v-theme-error), 0.08);
}
</style>
