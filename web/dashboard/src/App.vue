<template>
  <!-- Layout handles its own transitions; App just wraps router + snackbar -->
  <router-view />

  <!-- Global snackbar -->
  <v-snackbar
    v-model="snack.visible.value"
    :color="snack.current.value?.color"
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
      <v-btn variant="text" size="small" icon="mdi-close" @click="snack.dismiss()" />
    </template>
  </v-snackbar>
</template>

<script setup lang="ts">
import { useSnackbar } from '@/composables/useSnackbar'

const snack = useSnackbar()
</script>
