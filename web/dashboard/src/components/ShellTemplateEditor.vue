<template>
  <v-dialog :model-value="modelValue" max-width="760" @update:model-value="$emit('update:modelValue', $event)">
    <v-card class="pa-4">
      <div class="d-flex align-center mb-3">
        <v-icon size="16" color="primary" class="mr-2">mdi-console</v-icon>
        <code class="text-subtitle-2">{{ title }}</code>
        <v-spacer />
        <v-btn icon size="x-small" variant="text" @click="cancel">
          <v-icon size="16">mdi-close</v-icon>
        </v-btn>
      </div>

      <div ref="host" class="cm-host rounded mb-2" />

      <div class="d-flex align-center flex-wrap ga-1 mb-4">
        <span class="text-caption text-on-surface-variant mr-1">{{ t('editor.insert') }}</span>
        <v-chip
          v-for="ph in placeholders" :key="ph"
          size="x-small" variant="outlined" class="cursor-pointer"
          :color="ph.startsWith('param.') ? 'success' : undefined"
          style="font-family: monospace"
          :title="ph.startsWith('param.') ? 'task param (from your sweep / fixed_params)' : 'runq built-in'"
          @click="insertPlaceholder(ph)"
        >
          <v-icon start size="10">{{ ph.startsWith('param.') ? 'mdi-variable' : 'mdi-cog-outline' }}</v-icon>
          {{ '{{' + ph + '\}\}' }}
        </v-chip>
        <span class="text-caption text-on-surface-variant ml-2"><code>{{ '{{' }}</code> {{ t('editor.completion_hint') }}</span>
      </div>

      <div class="d-flex justify-end ga-2">
        <v-btn size="small" variant="text" @click="cancel">{{ t('common.cancel') }}</v-btn>
        <v-btn size="small" variant="tonal" color="primary" @click="apply">{{ t('common.apply') }}</v-btn>
      </div>
    </v-card>
  </v-dialog>
</template>

<script setup lang="ts">
import { ref, watch, nextTick, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import { useTheme } from 'vuetify'
import { EditorState } from '@codemirror/state'
import { EditorView, keymap, placeholder as cmPlaceholder } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap } from '@codemirror/commands'
import { autocompletion, completionKeymap, type CompletionContext } from '@codemirror/autocomplete'
import { StreamLanguage, syntaxHighlighting, defaultHighlightStyle } from '@codemirror/language'
import { shell } from '@codemirror/legacy-modes/mode/shell'

const props = withDefaults(defineProps<{
  modelValue: boolean
  value: string
  title: string
  placeholders: string[]
  hint?: string
}>(), { hint: '' })

const emit = defineEmits<{
  'update:modelValue': [open: boolean]
  apply: [value: string]
}>()

const { t } = useI18n()
const theme = useTheme()
const host = ref<HTMLElement>()
let view: EditorView | null = null

// {{ triggers completion with this field's vocabulary (single source:
// the list ships from the backend's Placeholders metadata).
function completions(ctx: CompletionContext) {
  const word = ctx.matchBefore(/\{\{[\w.]*$/)
  if (!word) return null
  return {
    from: word.from + 2,
    options: props.placeholders.map(ph => ({
      label: ph,
      type: ph.startsWith('param.') ? 'variable' : 'keyword',
      detail: ph.startsWith('param.') ? 'task param' : 'built-in',
      // param.* leaves the cursor after the dot for the user's own name
      apply: ph.endsWith('.*') ? ph.slice(0, -1) : ph + '}}',
    })),
  }
}

function createEditor() {
  destroyEditor()
  if (!host.value) return
  const isDark = theme.global.current.value.dark
  view = new EditorView({
    parent: host.value,
    state: EditorState.create({
      doc: props.value,
      extensions: [
        history(),
        keymap.of([...defaultKeymap, ...historyKeymap, ...completionKeymap]),
        StreamLanguage.define(shell).extension,
        syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
        autocompletion({ override: [completions], activateOnTyping: true }),
        EditorView.lineWrapping,
        cmPlaceholder(props.hint || 'shell command template...'),
        EditorView.theme({
          '&': { fontSize: '13px', minHeight: '96px' },
          '.cm-content': { fontFamily: 'monospace', padding: '10px 12px' },
          '.cm-scroller': { fontFamily: 'monospace' },
          '&.cm-focused': { outline: 'none' },
        }, { dark: isDark }),
      ],
    }),
  })
  view.focus()
}

function destroyEditor() {
  view?.destroy()
  view = null
}

watch(() => props.modelValue, (open) => {
  if (open) nextTick(createEditor)
  else destroyEditor()
})

onBeforeUnmount(destroyEditor)

function insertPlaceholder(ph: string) {
  if (!view) return
  const text = ph.endsWith('.*') ? '{{' + ph.slice(0, -1) : '{{' + ph + '}}'
  const { from, to } = view.state.selection.main
  view.dispatch({ changes: { from, to, insert: text }, selection: { anchor: from + text.length } })
  view.focus()
}

function apply() {
  emit('apply', view?.state.doc.toString() ?? props.value)
  emit('update:modelValue', false)
}

function cancel() {
  emit('update:modelValue', false)
}
</script>

<style scoped>
.cm-host {
  border: 1px solid rgb(var(--v-theme-outline-variant));
  background: rgb(var(--v-theme-surface));
  transition: border-color 0.15s ease;
}
.cm-host:focus-within { border-color: rgb(var(--v-theme-primary)); }
</style>
