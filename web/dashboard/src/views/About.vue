<template>
  <div class="d-flex justify-center" style="padding-top: 48px">
    <div style="max-width: 400px; width: 100%; text-align: center">
      <!-- Logo -->
      <RunqLogo :size="64" class="mb-4" />

      <ChibiMascot :size="72" mood="happy" variant="sparkle" class="mb-2" />

      <div class="text-h4 font-weight-bold mb-1">runq</div>
      <div class="text-body-1 text-on-surface-variant mb-6">{{ t('about.description') }}</div>

      <v-card class="pa-4 mb-4 text-left">
        <div class="d-flex flex-column ga-2">
          <div class="d-flex align-center justify-space-between">
            <span class="text-on-surface-variant">{{ t('settings.mode') }}</span>
            <v-chip size="small" variant="tonal" color="primary">{{ config.mode }}</v-chip>
          </div>
          <div class="d-flex align-center justify-space-between">
            <span class="text-on-surface-variant">Version</span>
            <code class="text-body-2">dev</code>
          </div>
        </div>
      </v-card>

      <div class="d-flex justify-center ga-2">
        <v-btn
          variant="tonal"
          size="small"
          href="https://github.com"
          target="_blank"
        >
          <v-icon start size="16">mdi-github</v-icon>
          GitHub
        </v-btn>
        <v-btn
          variant="tonal"
          size="small"
          href="#"
        >
          <v-icon start size="16">mdi-book-open-variant</v-icon>
          Docs
        </v-btn>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useConfigStore } from '@/stores/config'
import ChibiMascot from '@/components/ChibiMascot.vue'
import RunqLogo from '@/components/RunqLogo.vue'

const { t } = useI18n()
const config = useConfigStore()
</script>
