<template>
  <div class="target-grid">
    <!-- ── Target rail: comparing two clusters' submit lines is the common
         task — switching must not cost a page of navigation. ── -->
    <div class="rail">
      <div class="text-caption text-on-surface-variant px-2 mb-1">{{ t('target.rail') }}</div>
      <v-list-item
        v-for="item in targetItems" :key="item.name"
        :active="!creating && item.name === activeName"
        density="compact" rounded="lg" color="primary" class="mb-1"
        @click="openTarget(item.name)"
      >
        <template #prepend>
          <v-icon size="15">{{ isSchedulerCfg(item) ? 'mdi-server' : 'mdi-desktop-tower' }}</v-icon>
        </template>
        <v-list-item-title class="text-body-2">{{ item.name }}</v-list-item-title>
        <template #append>
          <span class="text-caption text-on-surface-variant">{{ railBadge(item) }}</span>
        </template>
      </v-list-item>
      <v-list-item
        density="compact" rounded="lg" color="primary"
        :active="creating"
        @click="openCreate"
      >
        <template #prepend><v-icon size="15">mdi-plus</v-icon></template>
        <v-list-item-title class="text-body-2">{{ t('target.add') }}</v-list-item-title>
      </v-list-item>

      <!-- RQ-75: generations of REMOVED targets, still tracking tasks -->
      <v-expansion-panels v-if="archivedGenerations.length > 0" variant="accordion" class="mt-3">
        <v-expansion-panel>
          <v-expansion-panel-title class="text-caption text-on-surface-variant">
            <v-icon size="14" start>mdi-archive-outline</v-icon>
            {{ t('settings.gen_archived_title', { n: archivedGenerations.length }) }}
          </v-expansion-panel-title>
          <v-expansion-panel-text>
            <div
              v-for="g in archivedGenerations" :key="g.target + g.generation"
              class="d-flex align-center ga-1 py-1 text-caption"
            >
              <v-icon size="12" :color="g.done_at ? 'grey' : 'warning'">
                {{ g.done_at ? 'mdi-check' : 'mdi-progress-clock' }}
              </v-icon>
              <code>{{ g.target }}</code>
              <span class="text-on-surface-variant">
                {{ g.done_at ? t('settings.gen_done') : t('settings.gen_tracking', { n: g.unfinished }) }}
              </span>
            </div>
          </v-expansion-panel-text>
        </v-expansion-panel>
      </v-expansion-panels>
    </div>

    <!-- ── One machine's config ── -->
    <div class="pane">
      <div class="d-flex align-center ga-2 mb-4 flex-wrap">
        <v-icon size="22" color="primary">{{ isScheduler ? 'mdi-server' : 'mdi-desktop-tower' }}</v-icon>
        <span class="text-h5 font-weight-bold">{{ creating ? t('target.add') : activeName }}</span>
        <!-- Kind is DERIVED (ssh/templates present), never a switch — an
             informational chip, not config. -->
        <v-chip v-if="!creating" size="x-small" variant="tonal">
          {{ isScheduler ? t('target.kind_scheduler') : t('target.kind_local') }}
        </v-chip>
        <v-spacer />
        <v-btn size="small" variant="tonal" :disabled="creating && !newName.trim()" @click="runCheck">
          <v-icon start size="14">mdi-stethoscope</v-icon>{{ t('settings.hpc_check') }}
        </v-btn>
        <v-btn
          size="small" variant="tonal" color="primary"
          :loading="saving"
          :disabled="creating ? !newName.trim() : !dirty"
          @click="save"
        >{{ creating ? t('target.create') : t('common.save') }}</v-btn>
      </div>

      <!-- RQ-75: previous generations of THIS target still tracking tasks -->
      <div
        v-for="g in retiringOfSelected" :key="g.generation"
        class="d-flex align-center ga-2 rounded pa-2 mb-3 retiring-row"
      >
        <v-icon size="14" color="warning">mdi-history</v-icon>
        <span class="text-caption">{{ t('settings.gen_retiring_row', { n: g.unfinished }) }}</span>
        <code class="text-caption text-on-surface-variant">{{ g.generation.slice(0, 8) }}</code>
        <span class="text-caption text-on-surface-variant ml-auto">{{ genDate(g.retired_at) }}</span>
      </div>

      <!-- ── Connection ── -->
      <v-card class="pa-5 mb-4">
        <div class="text-subtitle-2 mb-3">{{ t('target.connection') }}</div>
        <div class="conn-grid mb-3">
          <v-text-field
            v-if="creating"
            v-model="newName"
            :label="t('target.name')" placeholder="tsubame"
            density="compact" variant="outlined" hide-details class="font-mono"
          />
          <v-text-field
            v-model="form.sshHost"
            :label="t('target.ssh_host')" placeholder="login.cluster.ac.jp"
            :hint="t('target.ssh_host_hint')" persistent-hint
            density="compact" variant="outlined" class="font-mono"
          />
          <v-text-field
            v-model="form.sshUser"
            :label="t('target.ssh_user')" placeholder="user"
            density="compact" variant="outlined" hide-details class="font-mono"
            :disabled="!form.sshHost"
          />
          <v-text-field
            v-model="form.sshProxy"
            :label="t('target.ssh_proxy')" placeholder="bastion.example.org"
            density="compact" variant="outlined" hide-details class="font-mono"
            :disabled="!form.sshHost"
          />
          <v-text-field
            v-model="form.workspace"
            :label="t('target.workspace')" placeholder="/home/user/runq-work"
            :hint="t('target.workspace_hint')" persistent-hint
            density="compact" variant="outlined" class="font-mono"
          />
          <v-text-field
            v-model.number="form.maxInflight"
            :label="t('target.max_inflight')" type="number" min="0"
            :hint="t('target.max_inflight_hint')" persistent-hint
            density="compact" variant="outlined" class="font-mono"
          />
        </div>
        <div class="text-caption text-on-surface-variant mb-1">{{ t('target.env_setup') }} <code>env_setup</code></div>
        <v-textarea
          v-model="form.envSetup"
          placeholder="source ~/miniconda3/etc/profile.d/conda.sh"
          density="compact" variant="outlined" rows="2" auto-grow hide-details
          class="font-mono mb-1"
        />
        <div class="text-caption text-on-surface-variant">{{ t('target.env_setup_hint') }}</div>
      </v-card>

      <!-- ── Scheduler commands (schema-driven; every field always rendered) ── -->
      <v-card class="pa-5 mb-4">
        <div class="d-flex align-center ga-2 mb-1 flex-wrap">
          <div class="text-subtitle-2">{{ t('target.scheduler_cmds') }}</div>
          <v-spacer />
          <span v-if="presetNames.length" class="text-caption text-on-surface-variant">{{ t('settings.hpc_preset_label') }}</span>
          <v-chip
            v-for="p in presetNames" :key="p"
            size="x-small" variant="outlined" class="cursor-pointer font-mono"
            @click="loadPreset(p)"
          >{{ p }}</v-chip>
        </div>
        <div class="text-caption text-on-surface-variant mb-4">{{ t('settings.hpc_rewrite_note') }}</div>

        <template v-for="f in hpcFields" :key="f.key">
          <div class="d-flex align-baseline ga-2 mb-1">
            <span class="text-body-2 font-weight-medium">{{ t(f.labelKey) }}</span>
            <code class="text-caption text-on-surface-variant">{{ f.key }}</code>
          </div>
          <v-text-field
            v-if="f.key === 'submit_id_regex'"
            v-model="hpcForm[f.key]"
            :placeholder="f.placeholder"
            density="compact" variant="outlined"
            class="font-mono mb-3"
            :hint="t('settings.hpc_regex_hint')"
            persistent-hint
          />
          <template v-else>
            <div class="tmpl-display rounded pa-2 mb-1 cursor-pointer d-flex align-center ga-2" @click="openEditor(f.key)">
              <span class="font-mono flex-grow-1 text-body-2" :class="{ 'opacity-50 font-italic': !hpcForm[f.key] }" style="word-break: break-all">
                {{ hpcForm[f.key] || t('settings.tmpl_not_set') }}
              </span>
              <v-icon size="14" color="on-surface-variant" class="flex-shrink-0">mdi-pencil-outline</v-icon>
            </div>
            <div class="text-caption text-on-surface-variant mb-3">{{ t(f.hintKey) }}</div>
          </template>
        </template>

        <!-- status_parser: pipeline stages -->
        <div class="d-flex align-baseline ga-2 mb-1">
          <span class="text-body-2 font-weight-medium">{{ t('settings.hpc_f_parser') }}</span>
          <code class="text-caption text-on-surface-variant">status_parser</code>
        </div>
        <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.hpc_f_parser_hint') }}</div>
        <div v-for="(stage, i) in hpcParser" :key="i" class="d-flex ga-2 mb-1 align-center">
          <div class="tmpl-display rounded pa-2 cursor-pointer d-flex align-center ga-2 flex-grow-1" @click="openStageEditor(i)">
            <span class="font-mono flex-grow-1 text-body-2" :class="{ 'opacity-50': !stage }" style="word-break: break-all">
              {{ stage || `stage ${i + 1}` }}
            </span>
            <v-icon size="14" color="on-surface-variant" class="flex-shrink-0">mdi-pencil-outline</v-icon>
          </div>
          <v-btn icon size="x-small" variant="text" :aria-label="t('common.delete')" :title="t('common.delete')" @click="hpcParser.splice(i, 1)">
            <v-icon size="14">mdi-close</v-icon>
          </v-btn>
        </div>
        <v-btn size="x-small" variant="text" color="primary" @click="hpcParser.push(''); openStageEditor(hpcParser.length - 1)">
          <v-icon start size="12">mdi-plus</v-icon> {{ t('settings.hpc_add_stage') }}
        </v-btn>

        <ShellTemplateEditor
          v-model="editorOpen"
          :value="editorValue"
          :title="editorTitle"
          :placeholders="editorPlaceholders"
          :hint="editorHint"
          @apply="onEditorApply"
        />
      </v-card>

      <!-- ── Check results (same three-state grammar as preflight) ── -->
      <v-card v-if="hpcResults.length > 0" class="pa-5 mb-4">
        <div class="text-subtitle-2 mb-3">{{ t('target.check_results') }}</div>
        <div v-for="r in hpcResults" :key="r.name" class="d-flex align-start ga-2 py-1 text-caption">
          <v-icon size="14" :color="r.status === 'ok' ? 'success' : r.status === 'fail' ? 'error' : 'grey'">
            {{ r.status === 'ok' ? 'mdi-check' : r.status === 'fail' ? 'mdi-alert-circle' : 'mdi-minus' }}
          </v-icon>
          <code class="flex-shrink-0 check-name">{{ r.name }}</code>
          <span class="text-on-surface-variant font-mono" style="word-break: break-all">{{ r.detail }}</span>
        </div>
      </v-card>

      <!-- ── Remove ── -->
      <v-card v-if="!creating" class="pa-5">
        <div class="d-flex align-center ga-3">
          <div class="flex-grow-1">
            <div class="text-subtitle-2">{{ t('target.remove_title') }}</div>
            <div class="text-caption text-on-surface-variant mt-1">{{ t('target.remove_hint') }}</div>
          </div>
          <v-btn size="small" variant="tonal" color="error" :loading="removing" @click="removeTarget">
            {{ t('common.delete') }}
          </v-btn>
        </div>
      </v-card>
    </div>

    <!-- RQ-75: config.yaml changed on disk mid-edit — human arbitrates -->
    <ConfigConflictDialog
      v-model="conflictOpen"
      :fields="conflictFields"
      @use-disk="conflictUseDisk"
      @use-mine="conflictUseMine"
    />
  </div>
</template>

<script setup lang="ts">
// TargetPage (RQ2-4 ③, kit ScreensTarget) — everything that belongs to
// ONE machine. Target config used to sit inside global Settings, which
// put "how tsubame queues a job" next to "what theme the dashboard
// uses"; different owners, different blast radius. The RQ-75 machinery
// (config_generation If-Match, conflict dialog, focus disk-watch,
// unsaved guards) moves here with it. The kit's kind cards became a
// derived chip: in the real model, kind IS the fields (ssh/templates),
// not a switch that rewrites them.
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter, onBeforeRouteLeave } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfirm } from '@/composables/useConfirm'
import { isGenerationConflict } from '@/apis/client'
import { configApi, type TargetConfig, type TargetGenerationView, type HPCCheckResult } from '@/apis/config'
import { useConfigStore } from '@/stores/config'
import ShellTemplateEditor from '@/components/ShellTemplateEditor.vue'
import ConfigConflictDialog, { type ConflictField } from '@/components/ConfigConflictDialog.vue'

const props = defineProps<{ name?: string }>()
const route = useRoute()
const router = useRouter()
const { t } = useI18n()
const snack = useSnackbar()
const config = useConfigStore()
const { confirm: confirmDialog } = useConfirm()

// ── Load: items + placeholders + generation + generations ──
const targetItems = ref<TargetConfig[]>([])
const hpcPlaceholders = ref<Record<string, string[]>>({})
const configGeneration = ref('')
const targetGenerations = ref<TargetGenerationView[]>([])

async function reloadTargets(opts?: { silent?: boolean }) {
  const res = await configApi.listTargets(opts)
  targetItems.value = res.items ?? []
  hpcPlaceholders.value = res.placeholders ?? {}
  configGeneration.value = res.config_generation ?? ''
  targetGenerations.value = res.generations ?? []
}

// ── Selection: route param is the truth; ?new=1 = create mode ──
const creating = computed(() => route.query.new === '1')
const newName = ref('')
const activeName = computed(() => {
  if (creating.value) return ''
  const names = targetItems.value.map(x => x.name)
  return props.name && names.includes(props.name) ? props.name : names[0] ?? ''
})
const activeCfg = computed(() =>
  targetItems.value.find(x => x.name === activeName.value) ?? null)

function openTarget(name: string) {
  router.push({ name: 'target', params: { name } })
}
function openCreate() {
  router.push({ name: 'target', params: { name: props.name ?? '' }, query: { new: '1' } })
}

function isSchedulerCfg(cfg: TargetConfig): boolean {
  return !!(cfg.submit_template || cfg.scheduler)
}
const isScheduler = computed(() =>
  creating.value ? !!hpcForm.value.submit_template : !!activeCfg.value && isSchedulerCfg(activeCfg.value))
function railBadge(cfg: TargetConfig): string {
  if (cfg.scheduler) return String(cfg.scheduler)
  if (cfg.ssh) return 'ssh'
  return t('target.kind_local')
}

// ── Form state ──
type HPCFieldKey = 'submit_template' | 'submit_id_regex' | 'status_template' | 'kill_template'
const hpcFields: { key: HPCFieldKey; labelKey: string; hintKey: string; placeholder: string }[] = [
  { key: 'submit_template', labelKey: 'settings.hpc_f_submit', hintKey: 'settings.hpc_f_submit_hint', placeholder: 'sbatch --gpus={{gpus}} {{run_sh}}' },
  { key: 'submit_id_regex', labelKey: 'settings.hpc_f_regex', hintKey: 'settings.hpc_regex_hint', placeholder: 'Submitted batch job ([0-9]+)' },
  { key: 'status_template', labelKey: 'settings.hpc_f_status', hintKey: 'settings.hpc_f_status_hint', placeholder: 'sacct -n -X -j {{ext_id}} -o State' },
  { key: 'kill_template', labelKey: 'settings.hpc_f_kill', hintKey: 'settings.hpc_f_kill_hint', placeholder: 'scancel {{ext_id}}' },
]
const hpcForm = ref<Record<HPCFieldKey, string>>({
  submit_template: '', submit_id_regex: '', status_template: '', kill_template: '',
})
const hpcParser = ref<string[]>([])
const form = ref({
  sshHost: '', sshUser: '', sshProxy: '',
  workspace: '', envSetup: '', maxInflight: 0,
})
const hpcResults = ref<HPCCheckResult[]>([])

function populateForm() {
  const item = activeCfg.value
  hpcResults.value = []
  hpcForm.value = {
    submit_template: (item?.submit_template as string) || '',
    submit_id_regex: (item?.submit_id_regex as string) || '',
    status_template: (item?.status_template as string) || '',
    kill_template: (item?.kill_template as string) || '',
  }
  hpcParser.value = [...((item?.status_parser as string[]) || [])]
  const ssh = (item?.ssh ?? {}) as Record<string, unknown>
  form.value = {
    sshHost: (ssh.host as string) || '',
    sshUser: (ssh.user as string) || '',
    sshProxy: (ssh.proxy_jump as string) || '',
    workspace: (item?.workspace as string) || '',
    envSetup: (item?.env_setup as string) || '',
    maxInflight: (item?.max_inflight as number) || 0,
  }
}
watch([activeCfg, creating], populateForm, { immediate: true })

/** Merge the form over the stored config — unknown fields survive the
 *  read-modify-write round trip (index-signature contract). */
function collectTarget(): TargetConfig {
  const name = creating.value ? newName.value.trim() : activeName.value
  const base = (creating.value ? { name } : activeCfg.value) ?? { name }
  const prevSSH = (base.ssh ?? {}) as Record<string, unknown>
  const ssh = form.value.sshHost
    ? {
        ...prevSSH,
        host: form.value.sshHost,
        user: form.value.sshUser || undefined,
        proxy_jump: form.value.sshProxy || undefined,
      }
    : undefined
  return {
    ...base,
    name,
    ssh,
    workspace: form.value.workspace || undefined,
    env_setup: form.value.envSetup || undefined,
    max_inflight: form.value.maxInflight || undefined,
    submit_template: hpcForm.value.submit_template || undefined,
    submit_id_regex: hpcForm.value.submit_id_regex || undefined,
    status_template: hpcForm.value.status_template || undefined,
    status_parser: hpcParser.value.filter(s => s.trim()),
    kill_template: hpcForm.value.kill_template || undefined,
  }
}

// ── Dirty: the collected config diverges from the stored one. Whole-value
// compare — collect() spreads the stored config, so untouched unknown
// fields can never read as dirty. ──
function normalized(cfg: TargetConfig | null): string {
  if (!cfg) return ''
  const c = { ...cfg }
  if (!(c.status_parser as string[] | undefined)?.length) delete c.status_parser
  for (const k of Object.keys(c)) if (c[k] === undefined) delete c[k]
  return JSON.stringify(c, Object.keys(c).sort())
}
const dirty = computed(() =>
  creating.value ? true : normalized(collectTarget()) !== normalized(activeCfg.value))

// ── Presets (same source as `runq target config add --preset`) ──
const presetNames = ref<string[]>([])
const presetMap = ref<Record<string, TargetConfig>>({})
function loadPreset(name: string) {
  const p = presetMap.value[name]
  if (!p) return
  hpcForm.value = {
    submit_template: (p.submit_template as string) || '',
    submit_id_regex: (p.submit_id_regex as string) || '',
    status_template: (p.status_template as string) || '',
    kill_template: (p.kill_template as string) || '',
  }
  hpcParser.value = [...((p.status_parser as string[]) || [])]
  hpcResults.value = []
  snack.info(t('settings.hpc_preset_loaded', { name }))
}

// ── Shell editor dialog (one instance, retargeted per field/stage) ──
const editorOpen = ref(false)
const editorTitle = ref('')
const editorValue = ref('')
const editorPlaceholders = ref<string[]>([])
const editorHint = ref('')
let editorTarget: { kind: 'field'; key: HPCFieldKey } | { kind: 'stage'; index: number } | null = null

function placeholdersFor(key: string): string[] {
  const base = hpcPlaceholders.value[key] ?? []
  return key === 'submit_template' ? [...base, 'param.*'] : base
}
function openEditor(key: HPCFieldKey) {
  editorTarget = { kind: 'field', key }
  editorTitle.value = key
  editorValue.value = hpcForm.value[key]
  editorPlaceholders.value = placeholdersFor(key)
  editorHint.value = hpcFields.find(x => x.key === key)?.placeholder ?? ''
  editorOpen.value = true
}
function openStageEditor(index: number) {
  editorTarget = { kind: 'stage', index }
  editorTitle.value = `status_parser[${index}]`
  editorValue.value = hpcParser.value[index] ?? ''
  editorPlaceholders.value = placeholdersFor('status_parser')
  editorHint.value = ''
  editorOpen.value = true
}
function onEditorApply(value: string) {
  if (!editorTarget) return
  if (editorTarget.kind === 'field') hpcForm.value[editorTarget.key] = value
  else hpcParser.value[editorTarget.index] = value
  editorTarget = null
}

// ── Check / Save / Remove ──
const saving = ref(false)
const removing = ref(false)

async function runCheck() {
  const name = creating.value ? newName.value.trim() : activeName.value
  if (!name) return
  try {
    const res = await configApi.checkTarget(name, collectTarget())
    hpcResults.value = res.results
  } catch (e: any) {
    snack.error(e?.message || t('common.error'))
  }
}

async function save() {
  const name = creating.value ? newName.value.trim() : activeName.value
  if (!name) return
  saving.value = true
  try {
    await configApi.putTarget(name, collectTarget(), configGeneration.value)
    await reloadTargets()
    await config.fetchConfig() // rail/statusbar summaries follow the write
    if (creating.value) {
      snack.success(t('target.created', { name }))
      router.replace({ name: 'target', params: { name } })
    } else {
      populateForm()
      await runCheck()
      snack.success(t('settings.hpc_saved'))
    }
  } catch (e: any) {
    if (isGenerationConflict(e)) {
      await openConflict()
      return
    }
    snack.error(e?.message || t('common.error'))
  } finally {
    saving.value = false
  }
}

async function removeTarget() {
  const name = activeName.value
  if (!name) return
  const ok = await confirmDialog({
    title: t('target.remove_title'),
    body: t('target.remove_confirm', { name }),
    confirmText: t('common.delete'),
    danger: true,
  })
  if (!ok) return
  removing.value = true
  try {
    await configApi.deleteTarget(name, configGeneration.value)
    await reloadTargets()
    await config.fetchConfig()
    snack.success(t('target.removed', { name }))
    const next = targetItems.value[0]?.name
    if (next) router.replace({ name: 'target', params: { name: next } })
    else router.replace({ name: 'target', params: { name: '' }, query: { new: '1' } })
  } catch (e: any) {
    if (isGenerationConflict(e)) {
      // The file moved underneath — re-read and let the user retry the
      // delete against fresh state; a merge dialog makes no sense here.
      await reloadTargets()
      snack.warn(t('settings.config_changed_on_disk'))
    } else {
      snack.error(e?.message || t('common.error'))
    }
  } finally {
    removing.value = false
  }
}

// ── Generation conflict (RQ-75): human arbitrates ──
const conflictOpen = ref(false)
const conflictFields = ref<ConflictField[]>([])

async function openConflict() {
  try {
    await reloadTargets()
  } catch {
    snack.error(t('common.error'))
    return
  }
  const disk = activeCfg.value
  const mine = collectTarget()
  const rows: ConflictField[] = []
  const keys: (keyof TargetConfig)[] = [
    'submit_template', 'submit_id_regex', 'status_template', 'kill_template',
    'workspace', 'env_setup', 'max_inflight',
  ]
  for (const k of keys) {
    const d = String((disk?.[k] as string | number | undefined) ?? '')
    const m = String((mine[k] as string | number | undefined) ?? '')
    if (d !== m) rows.push({ key: String(k), disk: d, mine: m })
  }
  const dParser = ((disk?.status_parser as string[]) ?? []).join('\n')
  const mParser = (mine.status_parser as string[] ?? []).join('\n')
  if (dParser !== mParser) rows.push({ key: 'status_parser', disk: dParser, mine: mParser })
  const dSSH = JSON.stringify(disk?.ssh ?? {})
  const mSSH = JSON.stringify(mine.ssh ?? {})
  if (dSSH !== mSSH) rows.push({ key: 'ssh', disk: dSSH, mine: mSSH })
  conflictFields.value = rows
  conflictOpen.value = true
}
function conflictUseDisk() {
  conflictOpen.value = false
  populateForm()
}
async function conflictUseMine() {
  conflictOpen.value = false
  await save()
}

// ── RQ-75 generations of this target / removed targets ──
const retiringOfSelected = computed(() =>
  targetGenerations.value.filter(
    g => !g.done_at && g.target === activeName.value
      && targetItems.value.some(x => x.name === g.target),
  ))
const archivedGenerations = computed(() =>
  targetGenerations.value.filter(g => !targetItems.value.some(x => x.name === g.target)))
function genDate(unix: number): string {
  return new Date(unix * 1000).toLocaleString()
}

// ── Disk watch on focus: clean form follows the file; a dirty form is
// only WARNED — never clobbered. ──
async function onWindowFocus() {
  try {
    const res = await configApi.listTargets({ silent: true })
    const gen = res.config_generation ?? ''
    if (!gen || gen === configGeneration.value) return
    if (dirty.value && !creating.value) {
      snack.info(t('settings.config_changed_on_disk'))
      return
    }
    targetItems.value = res.items ?? []
    hpcPlaceholders.value = res.placeholders ?? {}
    configGeneration.value = gen
    targetGenerations.value = res.generations ?? []
    populateForm()
  } catch {
    /* unreachable — the health banner owns connectivity reporting */
  }
}

// ── Unsaved-changes guards ──
function onBeforeUnload(e: BeforeUnloadEvent) {
  if (creating.value ? !newName.value.trim() : !dirty.value) return
  e.preventDefault()
  e.returnValue = ''
}
onBeforeRouteLeave(() => {
  if (creating.value ? !newName.value.trim() : !dirty.value) return true
  return window.confirm(t('settings.unsaved_leave_confirm'))
})

onMounted(async () => {
  window.addEventListener('focus', onWindowFocus)
  window.addEventListener('beforeunload', onBeforeUnload)
  try {
    await reloadTargets()
  } catch (e: any) {
    snack.error(e?.message || t('common.error'))
  }
  try {
    const p = await configApi.targetPresets()
    presetNames.value = p.names ?? []
    presetMap.value = p.presets ?? {}
  } catch {
    /* presets are sugar */
  }
})
onUnmounted(() => {
  window.removeEventListener('focus', onWindowFocus)
  window.removeEventListener('beforeunload', onBeforeUnload)
})
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.cursor-pointer { cursor: pointer; }
.target-grid {
  display: grid;
  grid-template-columns: 200px minmax(0, 1fr);
  gap: 20px;
  max-width: 960px;
  margin: 0 auto;
  align-items: start;
}
.rail { position: sticky; top: 72px; }
@media (max-width: 959px) {
  .target-grid { grid-template-columns: 1fr; }
  .rail { position: static; }
}
.conn-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
}
@media (max-width: 700px) {
  .conn-grid { grid-template-columns: 1fr; }
}
.retiring-row { background: rgb(var(--v-theme-surface-variant), 0.25); }
.tmpl-display {
  border: 1px solid rgb(var(--v-theme-outline-variant));
  background: rgb(var(--v-theme-surface-variant), 0.2);
  min-height: 38px;
}
.check-name { width: 140px; }
.opacity-50 { opacity: 0.5; }
</style>
