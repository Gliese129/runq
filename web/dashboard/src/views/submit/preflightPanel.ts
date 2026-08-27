// Pure logic for the contract-aware preflight panel (RQ2-3 c5, ex-RQ-80).
import type { PreflightCheck } from '@/types/api'

/** Sort order for check rows: problems first, then the CI-pipeline read. */
const STATUS_RANK: Record<string, number> = { failed: 0, warning: 1, passed: 2, skipped: 3 }

export function orderChecks(checks: PreflightCheck[] | undefined): PreflightCheck[] {
  return [...(checks ?? [])].sort(
    (a, b) => (STATUS_RANK[a.status] ?? 9) - (STATUS_RANK[b.status] ?? 9),
  )
}
