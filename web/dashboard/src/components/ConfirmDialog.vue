<template>
  <v-dialog
    :model-value="visible"
    max-width="420"
    @update:model-value="(v: boolean) => { if (!v) settle(false) }"
  >
    <v-card>
      <v-card-title class="text-subtitle-1">{{ opts.title }}</v-card-title>
      <v-card-text v-if="opts.body" class="text-body-2">{{ opts.body }}</v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" size="small" @click="settle(false)">
          {{ opts.cancelText || t('common.cancel') }}
        </v-btn>
        <v-btn
          :color="opts.danger ? 'error' : 'primary'"
          variant="tonal"
          size="small"
          @click="settle(true)"
        >
          {{ opts.confirmText || t('common.confirm') }}
        </v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useConfirm } from '@/composables/useConfirm'

const { t } = useI18n()
const { visible, opts, settle } = useConfirm()
</script>
