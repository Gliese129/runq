import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { configApi } from '@/apis/config'
import type { Capabilities, TargetSummary } from '@/types/api'

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

/**
 * v1 bootstrap: multi-target model. `mode` is gone from the wire —
 * capabilities are declared per target. Until the multi-target shell
 * lands, currentTarget follows default_target (single-target UX).
 */
export const useConfigStore = defineStore('config', () => {
  const dataPath = ref('')
  const configPath = ref('')
  const defaultTarget = ref('')
  const targetState = ref<'configured' | 'unconfigured'>('unconfigured')
  const targets = ref<TargetSummary[]>([])
  const loaded = ref(false)

  // The target the UI is operating on. Alignment phase: = default_target.
  // The app-shell issue upgrades this to a user-switchable context.
  const currentTarget = computed(() => defaultTarget.value || targets.value[0]?.name || '')

  const currentTargetSummary = computed(() =>
    targets.value.find((t) => t.name === currentTarget.value),
  )

  /** Capabilities of the CURRENT target — existing caps-gating reads this. */
  const caps = computed<Capabilities>(
    () => currentTargetSummary.value?.capabilities ?? { ...NO_CAPS },
  )

  /** Human-readable descriptor shown where `mode` used to be. */
  const targetLabel = computed(() => {
    const t = currentTargetSummary.value
    if (!t) return ''
    return t.scheduler ? `${t.name} (${t.scheduler})` : t.name
  })

  // Convenience getters for the two non-boolean dimensions.
  const isPoll = computed(() => caps.value.state_model === 'poll')
  const killAsync = computed(() => caps.value.kill_async)

  async function fetchConfig() {
    const res = await configApi.get()
    dataPath.value = res.data_path
    configPath.value = res.config_path
    defaultTarget.value = res.default_target
    targets.value = res.targets ?? []
    targetState.value =
      res.target_state ?? (targets.value.length > 0 ? 'configured' : 'unconfigured')
    loaded.value = true
  }

  return {
    dataPath,
    configPath,
    defaultTarget,
    targetState,
    targets,
    loaded,
    currentTarget,
    currentTargetSummary,
    caps,
    targetLabel,
    isPoll,
    killAsync,
    fetchConfig,
  }
})
