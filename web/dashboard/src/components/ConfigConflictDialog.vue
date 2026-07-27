<template>
  <v-dialog
    :model-value="modelValue"
    max-width="640"
    @update:model-value="$emit('update:modelValue', $event)"
  >
    <v-card class="pa-1">
      <v-card-title class="d-flex align-center ga-2 text-subtitle-1">
        <v-icon size="18" color="warning">mdi-file-compare</v-icon>
        {{ t('settings.conflict_title') }}
      </v-card-title>
      <v-card-text>
        <div class="text-body-2 text-on-surface-variant mb-4">
          {{ t('settings.conflict_body') }}
        </div>

        <!-- Someone changed OTHER parts of the file; the fields being edited
             here don't overlap. Retrying is safe — say so instead of
             rendering an empty diff table. -->
        <div v-if="fields.length === 0" class="text-body-2 text-on-surface-variant">
          {{ t('settings.conflict_no_overlap') }}
        </div>

        <!-- Field-level diff: only the fields where disk and form disagree -->
        <div v-else class="conflict-table rounded">
          <div class="conflict-row conflict-head">
            <code class="conflict-key">{{ t('settings.conflict_field') }}</code>
            <span class="conflict-val text-caption">{{ t('settings.conflict_disk') }}</span>
            <span class="conflict-val text-caption">{{ t('settings.conflict_mine') }}</span>
          </div>
          <div v-for="f in fields" :key="f.key" class="conflict-row">
            <code class="conflict-key">{{ f.key }}</code>
            <span class="conflict-val font-mono conflict-disk">{{
              f.disk || t('settings.conflict_empty')
            }}</span>
            <span class="conflict-val font-mono conflict-mine">{{
              f.mine || t('settings.conflict_empty')
            }}</span>
          </div>
        </div>
      </v-card-text>
      <v-card-actions class="px-4 pb-3">
        <v-spacer />
        <v-btn variant="tonal" @click="$emit('use-disk')">{{
          t('settings.conflict_use_disk')
        }}</v-btn>
        <v-btn variant="flat" color="primary" :loading="saving" @click="$emit('use-mine')">
          {{ t('settings.conflict_use_mine') }}
        </v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'

/**
 * RQ-75 generation conflict resolver: config.yaml changed on disk between
 * form load and save (another client, an editor, the CLI). The HUMAN
 * arbitrates — per field the dialog shows disk vs form, then one choice:
 * adopt the disk version (drop my edits) or keep mine (retry the save
 * against the fresh generation, overwriting the disk edit).
 */
export interface ConflictField {
  key: string
  disk: string
  mine: string
}

defineProps<{
  modelValue: boolean
  fields: ConflictField[]
  saving?: boolean
}>()

defineEmits<{
  'update:modelValue': [value: boolean]
  'use-disk': []
  'use-mine': []
}>()

const { t } = useI18n()
</script>

<style scoped>
.conflict-table {
  border: 1px solid rgb(var(--v-theme-outline-variant));
  overflow: hidden;
}
.conflict-row {
  display: grid;
  grid-template-columns: 140px 1fr 1fr;
  gap: 8px;
  padding: 6px 10px;
  border-top: 1px solid rgb(var(--v-theme-outline-variant));
  align-items: baseline;
}
.conflict-row:first-child {
  border-top: none;
}
.conflict-head {
  background: rgb(var(--v-theme-surface-variant), 0.3);
}
.conflict-key {
  font-size: 12px;
  word-break: break-all;
}
.conflict-val {
  font-size: 12px;
  word-break: break-all;
  white-space: pre-wrap;
}
.conflict-disk {
  color: rgb(var(--v-theme-on-surface-variant));
}
.conflict-mine {
  color: rgb(var(--v-theme-primary));
}
.font-mono {
  font-family: monospace;
}
</style>
