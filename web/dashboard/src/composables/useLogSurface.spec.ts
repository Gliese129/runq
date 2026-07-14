// @vitest-environment happy-dom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { ref, nextTick, effectScope, type EffectScope, type Ref } from 'vue'
import { createPinia, setActivePinia } from 'pinia'
import { useLogSurface } from './useLogSurface'

type Surface = ReturnType<typeof useLogSurface>

async function search(surface: Surface, q: string) {
  surface.searchQuery.value = q
  await nextTick() // flush the debounce-scheduling watcher
  vi.advanceTimersByTime(300)
}

// Search behavior over the FLATTENED render items (RQ-54 layer E):
// expanded blocks contribute one candidate per block-line, so matches are
// line-granular instead of whole-block.
describe('useLogSurface search', () => {
  let scope: EffectScope

  beforeEach(() => {
    vi.useFakeTimers()
    setActivePinia(createPinia())
    scope = effectScope()
  })

  afterEach(() => {
    scope.stop()
    vi.useRealTimers()
  })

  function makeSurface(lines: string[]) {
    const logLines = ref(lines)
    const container = ref<HTMLElement | undefined>() as Ref<HTMLElement | undefined>
    return scope.run(() => useLogSurface(logLines, container))!
  }

  it('matches individual block-lines inside an expanded block', async () => {
    const surface = makeSurface([
      'INFO Epoch 1 loss=0.5 lr=0.001',
      'INFO Epoch 2 loss=0.4 lr=0.001',
      'INFO Epoch 3 loss=0.3 lr=0.001',
    ])
    // 3 similar lines flatten into block-head + 3 block-lines + block-tail
    expect(surface.renderItems.value.some(i => i.type === 'block-head')).toBe(true)

    await search(surface, 'Epoch 2')
    const matches = surface.searchMatches.value
    expect(matches).toHaveLength(1)
    const hit = surface.renderItems.value[matches[0]]
    expect(hit.type).toBe('block-line')
    if (hit.type === 'block-line') expect(hit.line.text).toContain('Epoch 2')
  })

  it('matches every block-line when the query hits the whole block', async () => {
    const surface = makeSurface([
      'INFO Epoch 1 loss=0.5 lr=0.001',
      'INFO Epoch 2 loss=0.4 lr=0.001',
      'INFO Epoch 3 loss=0.3 lr=0.001',
    ])
    await search(surface, 'loss=')
    // Line-granular: one match per block-line, not one per block
    expect(surface.searchMatches.value).toHaveLength(3)
  })

  it('reports regex errors without throwing', async () => {
    const surface = makeSurface(['plain line one', 'plain line two'])
    surface.searchRegex.value = true
    await search(surface, '[invalid')
    expect(surface.searchMatches.value).toHaveLength(0)
    expect(surface.searchError.value).not.toBe('')
  })
})
