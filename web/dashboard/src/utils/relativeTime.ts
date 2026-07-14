// Shared time formatting helpers.
//
// relativeTime replaces the three hand-rolled copies that lived in
// JobHeader / ProjectJobs / Overview ('just now / Xm ago'), using
// Intl.RelativeTimeFormat so the output follows the active locale.
// formatDuration is locale-neutral on purpose (pure number+unit codes).
import i18n from '@/plugins/i18n'

/** Cache one formatter per locale — construction is not free. */
const rtfCache = new Map<string, Intl.RelativeTimeFormat>()

function formatterFor(locale: string): Intl.RelativeTimeFormat {
  let rtf = rtfCache.get(locale)
  if (!rtf) {
    rtf = new Intl.RelativeTimeFormat(locale, { numeric: 'always', style: 'narrow' })
    rtfCache.set(locale, rtf)
  }
  return rtf
}

/**
 * Human relative time for a past unix timestamp (seconds).
 * Locale comes from the active vue-i18n locale.
 */
export function relativeTime(ts: number, nowMs: number = Date.now()): string {
  const locale = i18n.global.locale.value
  const diff = nowMs / 1000 - ts
  if (diff < 60) return i18n.global.t('time.just_now')
  const rtf = formatterFor(locale)
  if (diff < 3600) return rtf.format(-Math.floor(diff / 60), 'minute')
  if (diff < 86400) return rtf.format(-Math.floor(diff / 3600), 'hour')
  return rtf.format(-Math.floor(diff / 86400), 'day')
}

/**
 * Compact duration: "45s", "3m 20s", "2h 5m".
 * Number+unit codes only — intentionally not translated.
 */
export function formatDuration(sec: number): string {
  const s = Math.round(sec)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
