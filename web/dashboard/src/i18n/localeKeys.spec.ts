// Guard: the three locale files must expose the exact same key set.
// A key added to one file but not the others silently falls back to
// English at runtime — this test turns that drift into a CI failure.
import { describe, it, expect } from 'vitest'
import en from './en.json'
import zhCN from './zh-CN.json'
import ja from './ja.json'

/** Recursively flatten nested message objects into dot-joined key paths.
 *  Current files are flat, but the guard must survive a future nesting. */
function flattenKeys(obj: Record<string, unknown>, prefix = ''): string[] {
  const keys: string[] = []
  for (const [k, v] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${k}` : k
    if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
      keys.push(...flattenKeys(v as Record<string, unknown>, path))
    } else {
      keys.push(path)
    }
  }
  return keys
}

describe('i18n locale files', () => {
  const locales = { en, 'zh-CN': zhCN, ja } as const
  const enKeys = new Set(flattenKeys(en))

  it.each(Object.entries(locales))('%s has no empty messages', (_name, messages) => {
    for (const [key, value] of Object.entries(messages)) {
      expect(typeof value, `key ${key}`).toBe('string')
      expect((value as string).length, `key ${key} is empty`).toBeGreaterThan(0)
    }
  })

  it.each([['zh-CN', zhCN], ['ja', ja]] as const)(
    '%s key set matches en.json exactly',
    (_name, messages) => {
      const keys = new Set(flattenKeys(messages))
      const missing = [...enKeys].filter(k => !keys.has(k))
      const extra = [...keys].filter(k => !enKeys.has(k))
      expect(missing, 'keys missing (present in en.json)').toEqual([])
      expect(extra, 'extra keys (absent from en.json)').toEqual([])
    },
  )
})
