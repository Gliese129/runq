import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { api } from '@/apis/client'
import type { GPUSlot } from '@/types/api'

export const useGPUStore = defineStore('gpu', () => {
  const gpus = ref<GPUSlot[]>([])
  const loading = ref(false)

  const freeCount = computed(() => gpus.value.filter(g => !g.task_id).length)
  const totalCount = computed(() => gpus.value.length)

  async function fetchGPU(silent = false) {
    loading.value = true
    try {
      gpus.value = await api.get<GPUSlot[]>('/gpu', { silent })
    } catch {
      // swallow
    } finally {
      loading.value = false
    }
  }

  return { gpus, loading, freeCount, totalCount, fetchGPU }
})
