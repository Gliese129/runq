<template>
  <v-app>
    <!-- Same rail sidebar -->
    <v-navigation-drawer
      rail
      permanent
      color="surface"
      style="border-right: 1px solid rgba(0,0,0,0.06)"
    >
      <div class="d-flex flex-column align-center py-3">
        <router-link :to="{ name: 'overview' }" class="text-decoration-none mb-4">
          <RunqLogo :size="36" />
        </router-link>
      </div>

      <div class="d-flex flex-column align-center ga-1 px-1">
        <v-tooltip text="Back" location="end">
          <template #activator="{ props: tp }">
            <v-btn v-bind="tp" icon size="small" variant="text" @click="goBack">
              <v-icon size="20">mdi-arrow-left</v-icon>
            </v-btn>
          </template>
        </v-tooltip>
      </div>
    </v-navigation-drawer>

    <!-- Top bar with breadcrumb -->
    <v-app-bar elevation="0" color="transparent" style="border-bottom: 1px solid rgba(0,0,0,0.04)">
      <v-app-bar-title class="d-flex align-center ga-1">
        <router-link
          :to="{ name: 'project', params: { project: route.params.project } }"
          class="text-decoration-none text-on-surface-variant text-body-2"
        >
          {{ route.params.project }}
        </router-link>
        <v-icon size="14" color="on-surface-variant">mdi-chevron-right</v-icon>
        <code class="text-body-2 text-on-surface">{{ shortJobId }}</code>
      </v-app-bar-title>

      <template #append>
        <v-btn
          size="small"
          variant="tonal"
          color="primary"
          @click="startFromThis"
        >
          <v-icon start size="16">mdi-content-copy</v-icon>
          {{ t('job.clone') }}
        </v-btn>
      </template>
    </v-app-bar>

    <v-main>
      <v-container fluid class="pa-5 pa-md-8" style="max-width: 1200px">
        <router-view v-slot="{ Component, route: r }">
          <transition name="fade-slide" mode="out-in">
            <component :is="Component" :key="r.path" />
          </transition>
        </router-view>
      </v-container>
    </v-main>
  </v-app>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import RunqLogo from '@/components/RunqLogo.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const shortJobId = computed(() => {
  const id = route.params.jobId as string
  return id.length > 8 ? id.slice(0, 8) : id
})

function goBack() {
  router.push({ name: 'project', params: { project: route.params.project } })
}

function startFromThis() {
  router.push({ name: 'submit', query: { from: route.params.jobId as string } })
}
</script>
