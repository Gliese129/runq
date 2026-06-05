import assert from 'node:assert/strict'
import { Buffer } from 'node:buffer'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import ts from 'typescript'

const flow = await loadSubmitFlow()

test('submit flow survives clumsy back-and-forth edits', () => {
  const project = {
    name: 'vision-train',
    workDir: '/tmp/runq/vision-train',
    cmd: 'python train.py {{args}}',
    gpus: 1,
    maxRetry: 0,
    envType: 'venv',
    envPath: '.venv',
    envName: '',
    params: [
      {
        name: 'dataset',
        type: 'file',
        default: '/data/a.csv',
        include: true,
        values: ['/data/a.csv', '/data/b.csv'],
      },
      {
        name: 'lr',
        type: 'float',
        default: '0.01',
        include: true,
        values: [],
        min: 0,
        max: 1,
      },
      {
        name: 'seed',
        type: 'int',
        default: '42',
        include: true,
        values: [],
      },
      {
        name: 'debug',
        type: 'bool',
        default: 'false',
        include: false,
        values: [],
      },
    ],
  }

  const groups = []

  assert.equal(flow.totalTaskCount(groups), 1)
  assert.deepEqual(flow.validateConfigure(groups), { ok: true })
  assert.deepEqual(flow.buildJobConfig('vision-train', 'baseline', project, groups), {
    project: 'vision-train',
    note: 'baseline',
    fixed_params: {
      dataset: '/data/a.csv',
      lr: 0.01,
      seed: 42,
    },
    sweep: [],
  })

  const payload = flow.buildProjectPayload(project)
  assert.equal(payload.project_name, 'vision-train')
  assert.deepEqual(payload.params.find(p => p.name === 'dataset').choices, ['/data/a.csv', '/data/b.csv'])
  assert.equal(payload.params.find(p => p.name === 'debug').default, 'false')

  groups.push({
    id: 'g1',
    type: 'grid',
    expanded: true,
    params: [
      {
        name: 'dataset',
        type: 'file',
        default: '/data/a.csv',
        values: ['   '],
      },
    ],
  })

  assert.equal(flow.totalTaskCount(groups), 0)
  let validation = flow.validateConfigure(groups)
  assert.equal(validation.ok, false)
  assert.match(validation.message, /"dataset" has no values/)

  groups[0].params[0].values = ['/data/b.csv', '   ']
  assert.equal(flow.totalTaskCount(groups), 1)
  assert.deepEqual(flow.validateConfigure(groups), { ok: true })
  assert.deepEqual(flow.buildJobConfig('vision-train', 'dataset sweep', project, groups), {
    project: 'vision-train',
    note: 'dataset sweep',
    fixed_params: {
      lr: 0.01,
      seed: 42,
    },
    sweep: [
      {
        method: 'grid',
        parameters: {
          dataset: { values: ['/data/b.csv'] },
        },
      },
    ],
  })

  groups.splice(0, 1)
  project.params.find(p => p.name === 'lr').default = ' '

  assert.equal(flow.totalTaskCount(groups), 1)
  assert.deepEqual(flow.buildJobConfig('vision-train', 'back to defaults', project, groups), {
    project: 'vision-train',
    note: 'back to defaults',
    fixed_params: {
      dataset: '/data/a.csv',
      seed: 42,
    },
    sweep: [],
  })

  groups.push({
    id: 'g2',
    type: 'list',
    expanded: true,
    params: [
      {
        name: 'lr',
        type: 'float',
        default: ' ',
        values: ['0.1', '0.2'],
      },
      {
        name: 'seed',
        type: 'int',
        default: '42',
        values: ['1'],
      },
    ],
  })

  validation = flow.validateConfigure(groups, { listLengthMismatchMessage: 'mismatch' })
  assert.deepEqual(validation, { ok: false, message: 'mismatch' })

  groups[0].params.find(p => p.name === 'seed').values = ['abc', '2']
  validation = flow.validateConfigure(groups)
  assert.equal(validation.ok, false)
  assert.match(validation.message, /"seed" has invalid int value: abc/)

  groups[0].params.find(p => p.name === 'seed').values = ['1', '2']
  assert.deepEqual(flow.validateConfigure(groups), { ok: true })
  assert.equal(flow.totalTaskCount(groups), 2)
  assert.deepEqual(flow.sweepSummary(groups), '[List] lr(2), seed(2)')
  assert.deepEqual(flow.buildJobConfig('vision-train', 'fixed list run', project, groups), {
    project: 'vision-train',
    note: 'fixed list run',
    fixed_params: {
      dataset: '/data/a.csv',
    },
    sweep: [
      {
        method: 'list',
        parameters: {
          lr: { values: [0.1, 0.2] },
          seed: { values: [1, 2] },
        },
      },
    ],
  })

  const headers = flow.dryRunHeaders(
    [
      { seed: 1, z_extra: 'late', dataset: '/data/a.csv', lr: 0.1 },
    ],
    project.params,
  )
  assert.deepEqual(headers.map(h => h.key), ['dataset', 'lr', 'seed', 'z_extra'])
})

async function loadSubmitFlow() {
  const sourceUrl = new URL('../src/views/submit/submitFlow.ts', import.meta.url)
  const source = await readFile(sourceUrl, 'utf8')
  const transpiled = ts.transpileModule(source, {
    compilerOptions: {
      target: ts.ScriptTarget.ES2020,
      module: ts.ModuleKind.ES2020,
      importsNotUsedAsValues: ts.ImportsNotUsedAsValues.Remove,
    },
    fileName: sourceUrl.pathname,
  }).outputText
  const encoded = Buffer.from(transpiled, 'utf8').toString('base64')
  return import(`data:text/javascript;base64,${encoded}`)
}
