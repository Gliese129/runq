import { describe, it, expect } from 'vitest'
import { buildProjectPayload, type SubmitProjectDraft } from './submitFlow'
import type { ProjectConfig } from '@/types/api'

const draft = (over: Partial<SubmitProjectDraft> = {}): SubmitProjectDraft => ({
  name: 'p1',
  workDir: '/w',
  cmd: 'python train.py {{args}}',
  setupCmd: '',
  gpus: 1,
  maxRetry: 0,
  envType: '',
  envPath: '',
  envName: '',
  envText: '',
  jobName: '',
  params: [],
  ...over,
})

describe('buildProjectPayload (read-modify-write)', () => {
  // Regression: fields the form does not edit must survive a save.
  it('carries base-only fields through untouched', () => {
    const base: ProjectConfig = {
      project_name: 'p1',
      target: 'hpc-a',
      env_file: '.env.cluster',
      working_dir: '/w',
      command_template: 'python train.py {{args}}',
      defaults: { gpus_per_task: 2, max_retry: 1, timeout: '2h' },
      resume: { enabled: true, extra_args: '--resume' },
      wandb: { project: 'wb' },
    }
    const out = buildProjectPayload(draft({ gpus: 4, maxRetry: 3 }), base)
    expect(out.target).toBe('hpc-a')
    expect(out.env_file).toBe('.env.cluster')
    expect(out.defaults?.timeout).toBe('2h')
    expect(out.resume).toEqual({ enabled: true, extra_args: '--resume' })
    expect(out.wandb).toEqual({ project: 'wb' })
    // form-owned fields overwrite
    expect(out.defaults?.gpus_per_task).toBe(4)
    expect(out.defaults?.max_retry).toBe(3)
  })

  // Regression: gpus_per_task 0 (CPU-only) is legal and must round-trip —
  // `|| 1` in the form loader coerced it to 1 and the save clobbered base.
  it('preserves gpus_per_task = 0', () => {
    const out = buildProjectPayload(draft({ gpus: 0 }))
    expect(out.defaults?.gpus_per_task).toBe(0)
  })

  it('clearing python_env in the form clears it on the wire', () => {
    const base: ProjectConfig = {
      project_name: 'p1', working_dir: '/w', command_template: 'x',
      python_env: { type: 'conda', name: 'ml' },
    }
    const out = buildProjectPayload(draft({ envType: '' }), base)
    expect(out.python_env).toBeUndefined()
  })

  it('params are form-authoritative (empty list clears base params)', () => {
    const base: ProjectConfig = {
      project_name: 'p1', working_dir: '/w', command_template: 'x',
      params: [{ name: 'lr', type: 'float' }],
    }
    const out = buildProjectPayload(draft({ params: [] }), base)
    expect(out.params).toBeUndefined()
  })
})
