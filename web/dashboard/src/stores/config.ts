import { defineStore } from 'pinia'
import { ref } from 'vue'
import { configApi } from '@/apis/config'
import type { FeatureFlags } from '@/types/api'

export const useConfigStore = defineStore('config', () => {
  const mode = ref('')
  const dataPath = ref('')
  const configPath = ref('')
  const features = ref<FeatureFlags>({ gpu_map: false, pause_resume: false })
  const loaded = ref(false)

  async function fetchConfig() {
    const res = await configApi.get()
    mode.value = res.mode
    dataPath.value = res.data_path
    configPath.value = res.config_path
    features.value = res.features
    loaded.value = true
  }

  return { mode, dataPath, configPath, features, loaded, fetchConfig }
})
