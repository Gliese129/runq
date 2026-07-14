<template>
  <!-- Layout handles its own transitions; App just wraps router + snackbar -->
  <router-view />

  <!-- Global snackbar -->
  <v-snackbar
    v-model="snack.visible.value"
    :color="snack.current.value?.color"
    :timeout="snack.current.value?.timeout ?? 5000"
    location="bottom right"
    @update:model-value="(v: boolean) => { if (!v) snack.dismiss() }"
  >
    {{ snack.current.value?.text }}
    <template #actions>
      <v-btn
        v-if="snack.current.value?.action"
        variant="text"
        size="small"
        @click="snack.current.value?.onAction?.(); snack.dismiss()"
      >
        {{ snack.current.value?.action }}
      </v-btn>
      <v-btn variant="text" size="small" icon="mdi-close" :aria-label="t('common.close')" @click="snack.dismiss()" />
    </template>
  </v-snackbar>

  <!-- Global confirm dialog (useConfirm) -->
  <ConfirmDialog />
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useSnackbar } from '@/composables/useSnackbar'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

const { t } = useI18n()
const snack = useSnackbar()
</script>
