// Multi-line structure detection: tracebacks and pipe tables (step 3).
import type { TracebackBlock, TableBlock } from './types'
import { TB_START, TB_FILE, TB_ERROR, FRAMEWORK_PATH, TABLE_LINE_RE, TABLE_SEP_RE } from './lex'

// ── Step ③: Structure Detection (multi-line) ────────────────

export function findTracebacks(lines: string[]): TracebackBlock[] {
  const blocks: TracebackBlock[] = []
  let i = 0
  while (i < lines.length) {
    if (TB_START.test(lines[i])) {
      const start = i
      const userCode: number[] = []
      i++
      while (i < lines.length && i - start < 200) {
        const fm = TB_FILE.exec(lines[i])
        if (fm && !FRAMEWORK_PATH.test(fm[1])) userCode.push(i - start)
        if (TB_ERROR.test(lines[i])) {
          blocks.push({ start, end: i, errorMessage: lines[i], userCodeOffsets: userCode })
          i++
          break
        }
        i++
      }
    } else {
      i++
    }
  }
  return blocks
}

function parseTableCells(line: string): string[] {
  return line.trim().replace(/^\||\|$/g, '').split('|').map(c => c.trim())
}

export function findTableBlocks(texts: string[]): TableBlock[] {
  const blocks: TableBlock[] = []
  let i = 0
  while (i < texts.length) {
    if (TABLE_LINE_RE.test(texts[i])) {
      const start = i
      while (i < texts.length && TABLE_LINE_RE.test(texts[i])) i++
      const end = i - 1
      if (end > start) {
        const headers = parseTableCells(texts[start])
        const rows: string[][] = []
        for (let j = start + 1; j <= end; j++) {
          if (TABLE_SEP_RE.test(texts[j])) continue
          rows.push(parseTableCells(texts[j]))
        }
        blocks.push({ start, end, headers, rows })
      }
    } else {
      i++
    }
  }
  return blocks
}

