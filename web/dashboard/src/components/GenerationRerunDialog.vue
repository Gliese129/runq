<template>
  <v-dialog
    :model-value="modelValue"
    max-width="480"
    @update:model-value="$emit('update:modelValue', $event)"
  >
    <v-card class="pa-1">
      <v-card-title class="d-flex align-center ga-2 text-subtitle-1">
        <v-icon size="18" color="warning">mdi-alert-outline</v-icon>
        {{ t('task.gen_rerun_title') }}
      </v-card-title>
      <v-card-text>
        <div class="text-body-2 text-on-surface-variant mb-3">
          {{ t('task.gen_rerun_body') }}
        </div>
        <v-checkbox
          v-model="dontAsk"
          density="compact"
          hide-details
          :label="t('task.gen_rerun_dont_ask')"
        />
      </v-card-text>
      <v-card-actions class="px-4 pb-3">
        <!-- Cancel LEFT, rerun RIGHT (user-specified layout). -->
        <v-btn variant="tonal" @click="$emit('update:modelValue', false)">
          {{ t('common.cancel') }}
        </v-btn>
        <v-spacer />
        <v-btn variant="flat" color="primary" @click="$emit('confirm', dontAsk)">
          {{ t('task.gen_rerun_confirm') }}
        </v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'

// RQ-75: rerun of a task whose target config CHANGED since submission —
// the rerun will use the NEW config, and the human decides. The checkbox
// persists "don't ask again" via the settings store (handled by the
// caller on confirm).
defineProps<{ modelValue: boolean }>()
defineEmits<{ 'update:modelValue': [value: boolean]; confirm: [dontAskAgain: boolean] }>()

const { t } = useI18n()
const dontAsk = ref(false)
</script>
