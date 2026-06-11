<template>
  <v-dialog v-model="open" fullscreen persistent transition="dialog-bottom-transition">
    <v-card class="d-flex flex-column" style="border-radius: 0">
      <!-- ═══ Title bar ═══ -->
      <div class="d-flex align-center justify-space-between pa-3 px-4" style="border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant))">
        <div class="d-flex align-center ga-2">
          <v-icon size="18" color="primary">mdi-tune-variant</v-icon>
          <span class="text-subtitle-1 font-weight-medium">Parameters</span>
          <v-chip size="x-small" variant="tonal">{{ includedCount }} / {{ params.length }}</v-chip>
        </div>
        <div class="d-flex align-center ga-1">
          <v-btn size="small" variant="text" @click="($refs.fileInput as HTMLInputElement).click()">
            <v-icon start size="14">mdi-file-upload-outline</v-icon> Import
          </v-btn>
          <input ref="fileInput" type="file" accept=".yaml,.yml,.json" hidden @change="onFileSelect" />
          <v-btn icon size="small" variant="text" @click="cancel"><v-icon>mdi-close</v-icon></v-btn>
        </div>
      </div>

      <!-- Import preview banner -->
      <div v-if="importPreview" class="d-flex align-center ga-2 px-4 py-2" style="border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant)); background: rgb(var(--v-theme-surface-variant), 0.3)">
        <div class="text-caption flex-grow-1">
          <span class="font-weight-medium">{{ importPreview.length }}</span> params from {{ importFileName }}
          <span class="text-on-surface-variant ml-1">· Append overwrites on conflict</span>
        </div>
        <v-btn size="small" variant="tonal" color="primary" @click="confirmImport('replace')">Replace</v-btn>
        <v-btn size="small" variant="outlined" @click="confirmImport('append')">Append</v-btn>
        <v-btn size="x-small" icon variant="text" @click="importPreview = null"><v-icon size="14">mdi-close</v-icon></v-btn>
      </div>

      <!-- Filter bar -->
      <div class="d-flex align-center ga-2 px-4 py-2" style="border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant))">
        <v-text-field
          v-model="filter" placeholder="Filter..." prepend-inner-icon="mdi-magnify"
          density="compact" variant="outlined" hide-details clearable style="max-width: 220px"
        />
      </div>

      <!-- ═══ Inline table ═══ -->
      <div class="flex-grow-1 overflow-y-auto">
        <table class="param-table">
          <thead>
            <tr>
              <th style="width: 36px" title="Checked = appears in the next step's param table">Use</th>
              <th class="text-left">Name</th>
              <th style="width: 120px">Type</th>
              <th>Value / Default</th>
              <th style="width: 200px">Constraints</th>
              <th style="width: 36px"></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in filteredParams" :key="p.name" :class="{ 'row-excluded': !p.include }">
              <!-- Include -->
              <td><v-checkbox-btn v-model="p.include" density="compact" hide-details color="primary" /></td>
              <!-- Name -->
              <td><code>{{ p.name }}</code></td>
              <!-- Type -->
              <td>
                <v-select v-model="p.type" :items="paramTypeItems" density="compact" variant="plain" hide-details style="font-size: 12px" @update:model-value="autoInclude(p)" />
              </td>
              <!-- Value / Default -->
              <td>
                <!-- bool: three states — a bool without a default is a valid state -->
                <div v-if="p.type === 'bool'" class="d-flex ga-1">
                  <v-chip
                    v-for="opt in ['true', 'false']" :key="opt"
                    size="x-small"
                    :color="p.default === opt ? 'primary' : undefined"
                    :variant="p.default === opt ? 'flat' : 'outlined'"
                    @click="setBoolDefault(p, opt)"
                  >{{ opt }}</v-chip>
                  <span v-if="!p.default" class="text-caption text-on-surface-variant align-self-center">no default</span>
                </div>
                <!-- list: inline chips -->
                <div v-else-if="p.type === 'list'" class="d-flex flex-wrap align-center ga-1">
                  <v-chip v-for="(v, i) in (p.values || [])" :key="i" size="x-small" closable variant="tonal" @click:close="p.values!.splice(i, 1)">{{ v }}</v-chip>
                  <input
                    class="inline-add" placeholder="+ add"
                    @keydown.enter.prevent="addListFromInline(p, $event.target as HTMLInputElement)"
                  />
                </div>
                <!-- str/file/folder: default = values[0] -->
                <input
                  v-else-if="p.type === 'str' || p.type === 'file' || p.type === 'folder'"
                  :value="(p.values || [])[0] || ''"
                  @input="setValues0(p, ($event.target as HTMLInputElement).value)"
                  class="cell-input"
                  :placeholder="p.type === 'file' || p.type === 'folder' ? '/path/...' : '—'"
                />
                <!-- int/float/other: plain default -->
                <input
                  v-else
                  :value="p.default"
                  @input="setDefault(p, ($event.target as HTMLInputElement).value)"
                  class="cell-input"
                  placeholder="—"
                />
              </td>
              <!-- Constraints -->
              <td>
                <!-- int/float: min max -->
                <div v-if="p.type === 'int' || p.type === 'float'" class="d-flex align-center ga-1">
                  <input v-model.number="p.min" class="cell-input cell-sm" placeholder="min" type="number" @input="autoInclude(p)" />
                  <span class="text-on-surface-variant">–</span>
                  <input v-model.number="p.max" class="cell-input cell-sm" placeholder="max" type="number" @input="autoInclude(p)" />
                </div>
                <!-- str: values popover -->
                <v-menu v-else-if="p.type === 'str'" :close-on-content-click="false" location="bottom end">
                  <template #activator="{ props: menu }">
                    <v-btn v-bind="menu" size="x-small" variant="tonal" class="text-none">
                      {{ (p.values || []).length }} values
                      <v-icon end size="12">mdi-chevron-down</v-icon>
                    </v-btn>
                  </template>
                  <v-card class="pa-3" style="min-width: 280px; max-width: 400px">
                    <div class="d-flex align-center justify-space-between mb-2">
                      <span class="text-caption text-on-surface-variant">Selectable values — first is the default</span>
                      <v-checkbox-btn
                        v-model="p.strict"
                        density="compact" color="warning"
                        :title="'strict: values outside this list fail at submit'"
                        style="flex: none"
                      >
                        <template #label><span class="text-caption">strict</span></template>
                      </v-checkbox-btn>
                    </div>
                    <div class="d-flex flex-wrap ga-1 mb-2">
                      <v-chip
                        v-for="(val, i) in (p.values || [])" :key="i"
                        size="small" closable variant="tonal"
                        :color="i === 0 ? 'primary' : undefined"
                        @click:close="p.values!.splice(i, 1)"
                      >
                        {{ val }}<span v-if="i === 0" class="text-caption ml-1 opacity-60">default</span>
                      </v-chip>
                      <span v-if="!p.values?.length" class="text-caption text-on-surface-variant">None yet</span>
                    </div>
                    <div class="d-flex ga-1">
                      <v-text-field
                        v-model="valueInputs[p.name]"
                        placeholder="Add value + Enter"
                        density="compact" variant="underlined" hide-details
                        style="font-family: monospace; font-size: 12px"
                        @keydown.enter.prevent="addValue(p)"
                      />
                      <v-btn size="x-small" icon variant="text" color="primary" @click="addValue(p)">
                        <v-icon size="14">mdi-plus</v-icon>
                      </v-btn>
                    </div>
                  </v-card>
                </v-menu>
                <!-- file/folder: browse button (0 values) or popover (has values) -->
                <template v-else-if="p.type === 'file' || p.type === 'folder'">
                  <!-- No values yet → open file browser directly -->
                  <v-btn
                    v-if="!(p.values || []).length"
                    size="x-small" variant="tonal" class="text-none"
                    @click="openBrowseFor(p)"
                  >
                    <v-icon start size="12">mdi-folder-search-outline</v-icon> Browse
                  </v-btn>
                  <!-- Has values → popover with list + browse button -->
                  <v-menu v-else :close-on-content-click="false" location="bottom end">
                    <template #activator="{ props: menu }">
                      <v-btn v-bind="menu" size="x-small" variant="tonal" class="text-none">
                        {{ p.values!.length }} {{ p.type === 'file' ? 'files' : 'folders' }}
                        <v-icon end size="12">mdi-chevron-down</v-icon>
                      </v-btn>
                    </template>
                    <v-card class="pa-3" style="min-width: 280px; max-width: 400px">
                      <div class="text-caption text-on-surface-variant mb-2">Selectable paths</div>
                      <div class="d-flex flex-wrap ga-1 mb-2">
                        <v-chip v-for="(val, i) in p.values" :key="i" size="small" closable variant="tonal" @click:close="p.values!.splice(i, 1)">
                          {{ val.split('/').pop() || val }}
                          <v-tooltip activator="parent" location="top">{{ val }}</v-tooltip>
                        </v-chip>
                      </div>
                      <div class="d-flex ga-1">
                        <v-text-field v-model="valueInputs[p.name]" placeholder="Add path + Enter" density="compact" variant="underlined" hide-details style="font-family: monospace; font-size: 12px" @keydown.enter.prevent="addValue(p)" />
                        <v-btn size="x-small" icon variant="text" color="primary" @click="addValue(p)"><v-icon size="14">mdi-plus</v-icon></v-btn>
                        <v-btn size="x-small" icon variant="text" @click="openBrowseFor(p)"><v-icon size="14">mdi-folder-search-outline</v-icon></v-btn>
                      </div>
                    </v-card>
                  </v-menu>
                </template>
                <!-- list/bool: no constraints -->
              </td>
              <!-- Delete (independent of include — unchecked ≠ to-be-deleted) -->
              <td>
                <v-btn
                  icon size="x-small" variant="text" class="row-delete"
                  :aria-label="`Delete ${p.name}`" :title="`Delete ${p.name}`"
                  @click="removeParam(p.name)"
                >
                  <v-icon size="14" color="on-surface-variant">mdi-close</v-icon>
                </v-btn>
              </td>
            </tr>
          </tbody>
        </table>

        <div v-if="filteredParams.length === 0" class="text-center text-on-surface-variant pa-8">
          <div class="text-caption">{{ filter ? 'No matches' : 'No parameters — click Import above' }}</div>
        </div>
      </div>

      <!-- File browser for file/folder params -->
      <FileBrowserDialog
        v-model="showBrowser"
        :mode="browseTarget?.type === 'folder' ? 'folder' : 'file'"
        :file-filter="''"
        @select="onBrowseSelect"
      />

      <!-- ═══ Footer ═══ -->
      <div class="d-flex align-center justify-end pa-3 ga-2" style="border-top: 0.5px solid rgb(var(--v-theme-outline-variant))">
        <v-btn variant="text" @click="cancel">Cancel</v-btn>
        <v-btn variant="tonal" color="primary" @click="done">
          <v-icon start size="16">mdi-check</v-icon> Done
        </v-btn>
      </div>
    </v-card>
  </v-dialog>
</template>

<script setup lang="ts">
import { ref, reactive, computed, watch } from 'vue'
import * as YAML from 'js-yaml'
import { useSnackbar } from '@/composables/useSnackbar'
import { PARAM_TYPES, type ProjectParam } from '@/types/submit'
import FileBrowserDialog from '@/components/FileBrowserDialog.vue'

const props = defineProps<{
  modelValue: boolean
  params: ProjectParam[]
}>()

const emit = defineEmits<{
  'update:modelValue': [v: boolean]
  'update:params': [p: ProjectParam[]]
}>()

const snack = useSnackbar()
const paramTypeItems = [...PARAM_TYPES] as string[]

const open = computed({
  get: () => props.modelValue,
  set: v => emit('update:modelValue', v),
})

// ── State ──
const params = ref<ProjectParam[]>([])
const valueInputs = reactive<Record<string, string>>({})

watch(() => props.modelValue, (v) => {
  if (v) {
    params.value = props.params.map(p => {
      const copy = { ...p, values: p.values ? [...p.values] : [] } as ProjectParam
      // str/file/folder: ensure default is in values[0] (keep default intact)
      if (['str', 'file', 'folder'].includes(copy.type) && copy.default) {
        const values = copy.values || (copy.values = [])
        if (!values.includes(copy.default)) {
          values.unshift(copy.default)
        }
      }
      return copy
    })
    filter.value = ''
    importPreview.value = null
    Object.keys(valueInputs).forEach(k => delete valueInputs[k])
  }
})

const includedCount = computed(() => params.value.filter(p => p.include).length)

function done() {
  emit('update:params', params.value.map(p => {
    const copy = { ...p, values: p.values ? [...p.values] : [] }
    // Sync values[0] back to default for str/file/folder
    if (['str', 'file', 'folder'].includes(copy.type) && copy.values.length > 0) {
      copy.default = copy.values[0]
    }
    return copy
  }))
  open.value = false
}
function cancel() { open.value = false }

// ── Filter ──
const filter = ref('')
const filteredParams = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return params.value
  return params.value
    .map(p => ({ param: p, score: p.name.toLowerCase() === q ? 3 : p.name.toLowerCase().startsWith(q) ? 2 : p.name.toLowerCase().includes(q) ? 1 : 0 }))
    .filter(s => s.score > 0)
    .sort((a, b) => b.score - a.score)
    .map(s => s.param)
})

function removeParam(name: string) {
  params.value = params.value.filter(p => p.name !== name)
}

/** Auto-include only on actual edits (typing a value, picking a type) —
 *  never on focus, so unchecking sticks until the user changes something. */
function autoInclude(p: ProjectParam) {
  if (!p.include) p.include = true
}

// ── Value helpers ──

function setBoolDefault(p: ProjectParam, opt: string) {
  // Clicking the active state clears it back to "no default".
  p.default = p.default === opt ? '' : opt
  if (p.default) autoInclude(p)
}

function setDefault(p: ProjectParam, val: string) {
  p.default = val
  if (val.trim()) autoInclude(p)
}

/** For str/file/folder: default = values[0]. Editing default edits values[0]. */
function setValues0(p: ProjectParam, val: string) {
  if (!p.values) p.values = []
  if (p.values.length === 0) p.values.push(val)
  else p.values[0] = val
  if (val.trim()) autoInclude(p)
}

function addValue(p: ProjectParam) {
  const val = (valueInputs[p.name] || '').trim()
  if (!val) return
  if (!p.values) p.values = []
  if (!p.values.includes(val)) p.values.push(val)
  valueInputs[p.name] = ''
}

function addListFromInline(p: ProjectParam, input: HTMLInputElement) {
  const raw = input.value.trim()
  if (!raw) return
  if (!p.values) p.values = []
  for (const item of raw.split(/[,;\s]+/).map(s => s.trim()).filter(Boolean)) {
    if (!p.values.includes(item)) p.values.push(item)
  }
  input.value = ''
}

// ── File browser for file/folder params ──
const showBrowser = ref(false)
const browseTarget = ref<ProjectParam | null>(null)

function openBrowseFor(p: ProjectParam) {
  browseTarget.value = p
  showBrowser.value = true
}

function onBrowseSelect(path: string) {
  if (!browseTarget.value) return
  if (!browseTarget.value.values) browseTarget.value.values = []
  if (!browseTarget.value.values.includes(path)) {
    browseTarget.value.values.push(path)
  }
  browseTarget.value = null
}

// ── Import ──
const importPreview = ref<ProjectParam[] | null>(null)
const importFileName = ref('')

function onFileSelect(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (file) parseImportFile(file)
  ;(e.target as HTMLInputElement).value = ''
}

async function parseImportFile(file: File) {
  importFileName.value = file.name
  try {
    const text = await file.text()
    let data: any
    if (file.name.endsWith('.json')) data = JSON.parse(text)
    else data = YAML.load(text)
    const arr = Array.isArray(data) ? data : (data?.params || data?.parameters || [])
    if (!Array.isArray(arr) || arr.length === 0) { snack.error('No params found'); return }
    importPreview.value = arr.map((p: any) => {
      const values = Array.isArray(p.values)
        ? p.values
        : Array.isArray(p.choices)
          ? p.choices
          : []
      return {
        name: String(p.name || ''), type: String(p.type || 'str'),
        default: String(p.default ?? ''), include: p.include !== false,
        min: p.min, max: p.max,
        values: values.map(String),
      }
    }).filter((p: ProjectParam) => p.name)
  } catch (e: any) { snack.error(`Parse error: ${e.message}`) }
}

function confirmImport(mode: 'replace' | 'append') {
  if (!importPreview.value) return
  if (mode === 'replace') { params.value = importPreview.value }
  else {
    const m = new Map(params.value.map(p => [p.name, p]))
    for (const p of importPreview.value) m.set(p.name, p)
    params.value = Array.from(m.values())
  }
  importPreview.value = null
  snack.success(`${mode === 'replace' ? 'Replaced' : 'Merged'} ${params.value.length} params`)
}
</script>

<style scoped>
.param-table {
  width: 100%;
  border-collapse: collapse;
  font-family: 'JetBrains Mono', 'SF Mono', 'Fira Code', monospace;
  font-size: 13px;
}
.param-table th {
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
  font-size: 11px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.03em;
  color: rgb(var(--v-theme-on-surface-variant));
  padding: 8px 12px;
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
  position: sticky;
  top: 0;
  background: rgb(var(--v-theme-surface));
  z-index: 1;
}
.param-table td {
  padding: 6px 12px;
  border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant), 0.4);
  vertical-align: middle;
}
.param-table tbody tr:hover { background: rgb(var(--v-theme-surface-variant), 0.3); }
.row-excluded { opacity: 0.45; }
.row-delete { opacity: 0; transition: opacity 0.15s ease; }
.param-table tbody tr:hover .row-delete { opacity: 1; }
.cell-input {
  width: 100%;
  border: none;
  background: transparent;
  outline: none;
  font: inherit;
  color: inherit;
  padding: 2px 0;
  border-bottom: 1px solid transparent;
}
.cell-input:focus { border-bottom-color: rgb(var(--v-theme-primary)); }
.cell-input::placeholder { color: rgb(var(--v-theme-on-surface-variant)); opacity: 0.5; }
.cell-sm { max-width: 70px; text-align: center; }
.inline-add {
  border: none; background: transparent; outline: none;
  font-size: 11px; width: 50px; color: rgb(var(--v-theme-primary));
  cursor: text;
}
.inline-add::placeholder { color: rgb(var(--v-theme-primary)); opacity: 0.6; }
</style>
