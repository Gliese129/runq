import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { configApi } from '@/apis/config'
import type { Capabilities } from '@/types/api'

const NO_CAPS: Capabilities = {
  gpu_map: false,
  pause_resume: false,
  live_log: false,
  retry: false,
  state_model: 'push',
  kill_async: false,
  submit_preview: false,
  activity_heatmap: false,
  log_search: false,
}

export const useConfigStore = defineStore('config', () => {
  const mode = ref('')
  const dataPath = ref('')
  const configPath = ref('')
  const caps = ref<Capabilities>({ ...NO_CAPS })
  const loaded = ref(false)

  // Convenience getters for the two non-boolean dimensions.
  const isPoll = computed(() => caps.value.state_model === 'poll')
  const killAsync = computed(() => caps.value.kill_async)

  async function fetchConfig() {
    const res = await configApi.get()
    mode.value = res.mode
    dataPath.value = res.data_path
    configPath.value = res.config_path
    caps.value = res.capabilities ?? { ...NO_CAPS }
    loaded.value = true
  }

  return { mode, dataPath, configPath, caps, isPoll, killAsync, loaded, fetchConfig }
})
