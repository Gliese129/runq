// targetConfig.spec — pins the dirty-comparison contract, above all the
// regression Codex F1 caught: a nested ssh edit MUST read as different
// (the array-replacer version silently filtered nested keys away).
import { describe, it, expect } from 'vitest'
import { canonicalTarget } from './targetConfig'

const base = {
  name: 't1',
  workspace: '/w',
  ssh: { host: '127.0.0.1', user: 'u', proxy_jump: 'bastion' },
  submit_template: 'sbatch {{run_sh}}',
}

describe('canonicalTarget', () => {
  it('a nested ssh field change is a different config (Codex F1)', () => {
    for (const patch of [{ host: '127.0.0.2' }, { user: 'other' }, { proxy_jump: 'b2' }]) {
      const edited = { ...base, ssh: { ...base.ssh, ...patch } }
      expect(canonicalTarget(edited)).not.toBe(canonicalTarget(base))
    }
  })

  it('key order and undefined fields never read as dirty', () => {
    const reordered = {
      submit_template: 'sbatch {{run_sh}}',
      ssh: { proxy_jump: 'bastion', host: '127.0.0.1', user: 'u' },
      name: 't1',
      workspace: '/w',
      status_template: undefined,
    }
    expect(canonicalTarget(reordered as any)).toBe(canonicalTarget(base))
  })

  it('an empty string equals an absent field (Go serializes ssh.user: "")', () => {
    const disk = { ...base, ssh: { host: '127.0.0.1', user: '' } }
    const mine = { ...base, ssh: { host: '127.0.0.1' } }
    expect(canonicalTarget(disk as any)).toBe(canonicalTarget(mine as any))
  })

  it('an empty status_parser equals an absent one', () => {
    expect(canonicalTarget({ ...base, status_parser: [] } as any)).toBe(canonicalTarget(base))
    expect(canonicalTarget({ ...base, status_parser: ['grep x'] } as any)).not.toBe(canonicalTarget(base))
  })

  it('unknown fields participate in the comparison (round-trip safety)', () => {
    expect(canonicalTarget({ ...base, custom_flag: true } as any)).not.toBe(canonicalTarget(base))
  })
})
