// Lexical layer: regexes, ANSI/timestamp helpers, \r folding and
// per-line annotation (pipeline steps 1-2).
import type { LineTag, MetricMatch } from './types'

// ── Constants ────────────────────────────────────────────────

const TQDM_RE = /\d+%\|[^|]*\|\s*\d+\/\d+/
export const TABLE_LINE_RE = /^\s*\|.*\|\s*$/
export const TABLE_SEP_RE = /^\s*\|[-:| ]+\|\s*$/
const TIMESTAMP_RE = /^\[?(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[,.\d]*)\]?\s*/
export const RANK_RE = /\[(?:Rank|rank|GPU|Worker)[- ]?(\d+)\]/i
const METRIC_SRC = /\b(\w+)\s*=\s*(\d+\.?\d*(?:[eE][+-]?\d+)?)\b/.source
export const TB_START = /Traceback \(most recent call last\):/
export const TB_FILE  = /^\s+File "([^"]+)"/
export const TB_ERROR = /^\w+(?:Error|Exception|Warning):/
export const FRAMEWORK_PATH = /site-packages|lib\/python|<frozen|<string>/
const LEVEL_RULES: Array<[RegExp, LineTag]> = [
  [/\bERROR\b|\bCRITICAL\b|\bFATAL\b/,  'error'],
  [/\bWARNING\b|\bWARN\b/,               'warning'],
  [/\bDEBUG\b|\bTRACE\b/,                'debug'],
  [/\bINFO\b/,                           'info'],
]


// eslint-disable-next-line no-control-regex
const ANSI_RE = /\x1b\[[0-9;]*[A-Za-z]|\x1b\].*?(?:\x07|\x1b\\)/g
export function stripAnsi(line: string): string {
  return line.replace(ANSI_RE, '')
}

const RANK_COLORS = [
  '#22d3ee', '#a78bfa', '#f472b6', '#fb923c',
  '#34d399', '#fbbf24', '#60a5fa', '#e879f9',
]

export function formatTimestamp(ts: string): string {
  const m = /(\d{2}:\d{2}:\d{2})/.exec(ts)
  return m ? m[1] : ts
}

export function rankColor(rank: number): string {
  if (rank < 0) return ''
  return RANK_COLORS[rank % RANK_COLORS.length]
}

// ── Step ①: \r Folder ───────────────────────────────────────

export function applyCrFolder(lines: string[]): {
  lines: string[]
  tqdmFoldMap: Map<number, number>
} {
  const result: string[] = []
  const tqdmFoldMap = new Map<number, number>()
  let tqdmBuf: string[] = []

  const flush = () => {
    if (tqdmBuf.length === 0) return
    const idx = result.length
    result.push(tqdmBuf[tqdmBuf.length - 1])
    if (tqdmBuf.length > 1) tqdmFoldMap.set(idx, tqdmBuf.length - 1)
    tqdmBuf = []
  }

  for (const raw of lines) {
    let line = raw
    if (raw.includes('\r')) {
      const segs = raw.split('\r').filter(Boolean)
      if (segs.length > 0) line = segs[segs.length - 1]
    }
    if (TQDM_RE.test(line)) {
      tqdmBuf.push(line)
    } else if (line.trim() === '' && tqdmBuf.length > 0) {
      // absorb blank lines between tqdm updates
    } else {
      flush()
      result.push(line)
    }
  }
  flush()
  return { lines: result, tqdmFoldMap }
}

// ── Step ②: Annotate (per-line) ─────────────────────────────

interface AnnotatedLine {
  text: string          // timestamp-stripped display text
  timestamp: string
  tags: Set<LineTag>
  rank: number
  metrics: MetricMatch[]
  tqdmFolded: number
}

export function annotateLine(raw: string, tqdmFolded: number, crFolderOn: boolean): AnnotatedLine {
  let text = raw
  let timestamp = ''
  const tsm = TIMESTAMP_RE.exec(raw)
  if (tsm) { timestamp = tsm[1]; text = raw.slice(tsm[0].length) }

  const tags = new Set<LineTag>()

  // Level (non-exclusive with everything)
  for (const [re, tag] of LEVEL_RULES) {
    if (re.test(text)) { tags.add(tag); break }
  }

  // Rank
  let rank = -1
  const rm = RANK_RE.exec(text)
  if (rm) rank = parseInt(rm[1], 10)

  // Metrics
  const metrics: MetricMatch[] = []
  const mre = new RegExp(METRIC_SRC, 'g')
  let mm: RegExpExecArray | null
  while ((mm = mre.exec(text)) !== null) {
    metrics.push({ start: mm.index, end: mm.index + mm[0].length, key: mm[1], value: mm[2] })
  }
  if (metrics.length > 0) tags.add('metric')

  // tqdm
  const isTqdm = tqdmFolded > 0 || (crFolderOn && TQDM_RE.test(raw))
  if (isTqdm) tags.add('tqdm')

  return { text, timestamp, tags, rank, metrics, tqdmFolded }
}

