import { resolve } from 'node:path'
import { spawnSync } from 'node:child_process'
import { describe, expect, it } from 'vitest'
import {
  fileDeclaresTest,
  laneForFile,
  postmortemFiles,
  validateCorpusSchema,
  validateCorpusTree,
} from './verify-regression-corpus.ts'

const scriptPath = resolve(import.meta.dirname, 'verify-regression-corpus.ts')

type RawEntry = Record<string, unknown>

function corpusWith(entries: RawEntry[]): unknown {
  return {
    version: 1,
    bugClasses: [{ id: 'sample-class', statement: 'A sample class statement.' }],
    entries: [
      {
        id: '0001',
        postmortem: 'docs/postmortem/0001-acp-default-export-drops-inject.md',
        bugClass: 'sample-class',
        pinnedTests: [
          {
            file: 'packages/core/agent-loop/tests/contract-regressions.spec.ts',
            name: 'narrows event.data from event.type',
            lane: 'unit',
          },
        ],
        detectors: [],
      },
      ...entries,
    ],
  }
}

describe('laneForFile', () => {
  it('maps each lane include pattern to its vitest config', () => {
    expect(laneForFile('packages/core/session/tests/session.spec.ts')).toBe('unit')
    expect(laneForFile('apps/cli/tests/args.spec.ts')).toBe('unit')
    expect(laneForFile('apps/cli/tests/profiles/acp/tests/acp.e2e.ts')).toBe('e2e')
    expect(laneForFile('apps/web/tests/vite-entry.e2e.ts')).toBe('web')
    expect(laneForFile('packages/llm/llm/tests/adapter.expected.e2e.ts')).toBe('expected')
    expect(laneForFile('snapshots/session/headless/x.snapshot.ts')).toBe('snapshot')
    expect(laneForFile('docs/postmortem/0001.md')).toBeUndefined()
  })
})

describe('fileDeclaresTest', () => {
  it('matches the it/test declaration and rejects renames', () => {
    expect(fileDeclaresTest('packages/core/agent-loop/tests/contract-regressions.spec.ts', 'narrows event.data from event.type')).toBe(true)
    expect(fileDeclaresTest('packages/core/agent-loop/tests/contract-regressions.spec.ts', 'no such test')).toBe(false)
  })
})

describe('validateCorpusSchema', () => {
  it('accepts a well-formed corpus', () => {
    const { problems } = validateCorpusSchema(corpusWith([]))
    expect(problems).toEqual([])
  })

  it('rejects a wrong version, unknown bug class, and empty pins', () => {
    const { problems } = validateCorpusSchema(corpusWith([
      { id: '0002', bugClass: 'undeclared-class' },
      { id: '0003', pinnedTests: [] },
    ]))
    expect(problems.some(problem => problem.includes('version must be 1')))
    expect(problems.some(problem => problem.includes('must name a declared bug class'))).toBe(true)
    expect(problems.some(problem => problem.includes('non-empty "pinnedTests"'))).toBe(true)
  })

  it('rejects duplicate entry ids and a pin with a lane outside the vocabulary', () => {
    const { problems } = validateCorpusSchema(corpusWith([
      { id: '0001' },
      { id: '0009', pinnedTests: [{ file: 'x.spec.ts', name: 'y', lane: 'cli' }] },
    ]))
    expect(problems.some(problem => problem.includes('duplicate entry id'))).toBe(true)
    expect(problems.some(problem => problem.includes('"lane" must be one of'))).toBe(true)
  })
})

describe('validateCorpusTree', () => {
  it('reports every postmortem without an entry', () => {
    const { corpus } = validateCorpusSchema({
      version: 1,
      bugClasses: [{ id: 'c', statement: 's' }],
      entries: [],
    })
    const violations = validateCorpusTree(corpus)
    expect(postmortemFiles().length).toBeGreaterThan(0)
    expect(violations).toHaveLength(postmortemFiles().length)
    expect(violations[0]!.problem).toContain('has no corpus entry')
  })

  it('reports missing files, lane drift, and renamed tests', () => {
    const { corpus } = validateCorpusSchema(corpusWith([
      {
        id: '0002',
        postmortem: 'docs/postmortem/does-not-exist.md',
        pinnedTests: [
          { file: 'packages/core/agent-loop/tests/nope.spec.ts', name: 'gone', lane: 'unit' },
          { file: 'packages/core/agent-loop/tests/contract-regressions.spec.ts', name: 'a test that was renamed away', lane: 'unit' },
        ],
      },
    ]))
    const violations = validateCorpusTree(corpus)
    expect(violations.some(v => v.problem.includes('postmortem "docs/postmortem/does-not-exist.md" does not exist'))).toBe(true)
    expect(violations.some(v => v.problem.includes('pinned test file "packages/core/agent-loop/tests/nope.spec.ts" does not exist'))).toBe(true)
    expect(violations.some(v => v.problem.includes('renamed or removed'))).toBe(true)
  })

  it('reports a detector script that no longer exists', () => {
    const { corpus } = validateCorpusSchema(corpusWith([
      { id: '0002', detectors: ['scripts/absent-gate.ts'] },
    ]))
    const violations = validateCorpusTree(corpus)
    expect(violations.some(v => v.problem.includes('detector "scripts/absent-gate.ts" does not exist'))).toBe(true)
  })
})

describe('command-line entry', () => {
  it('passes on the shipped corpus', () => {
    const result = spawnSync('pnpm', ['exec', 'tsx', scriptPath], { encoding: 'utf8', timeout: 120_000 })
    expect(result.status).toBe(0)
    expect(result.stdout).toContain('all resolve')
  })
})
