import { ref } from 'vue'
import { useSettingsStore } from '@/stores/settings'

/**
 * RQ-75 cross-generation rerun flow, shared by TaskDetail and the job
 * task table: run() tries the retry; a 409 generation_changed (target
 * config changed since submission) opens the confirmation dialog unless
 * the user opted out ("don't ask again") — then it confirms directly.
 */
export function useGenerationRerun(
  retry: (p: { taskId: string; confirm?: boolean }) => Promise<unknown>,
  onSuccess: () => void,
  onError: (e: unknown) => void,
) {
  const settings = useSettingsStore()
  const open = ref(false)
  const pendingId = ref('')

  async function run(taskId: string) {
    try {
      await retry({ taskId, confirm: settings.skipGenRerunPrompt })
      onSuccess()
    } catch (e: any) {
      if (e?.code === 'generation_changed') {
        pendingId.value = taskId
        open.value = true
        return
      }
      onError(e)
    }
  }

  /** The user chose "rerun" in the dialog. */
  async function confirmRerun(dontAskAgain: boolean) {
    open.value = false
    if (dontAskAgain) settings.setSkipGenRerunPrompt(true)
    try {
      await retry({ taskId: pendingId.value, confirm: true })
      onSuccess()
    } catch (e) {
      onError(e)
    }
  }

  return { open, run, confirmRerun }
}
