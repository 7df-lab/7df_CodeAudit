/**
 * Enforce the regression corpus: every postmortem pins at least one keyless,
 * permanently running test (or gate script) that fails when its bug class
 * reintroduces, and every pin names a test that exists in the lane it claims.
 *
 * A postmortem documents why a bug escaped; the corpus makes the escape
 * route's guardrail mechanically required. Adding `docs/postmortem/NNNN-*.md`
 * without a matching entry fails this gate, as does a pin whose test file,
 * test name, or lane no longer matches the tree.
 *
 * Run: `tsx scripts/verify-regression-corpus.ts` (validate)
 *      `tsx scripts/verify-regression-corpus.ts --run` (execute unit/e2e pins)
 */

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
import { spawnSync } from 'node:child_process'

const root = resolve(import.meta.dirname, '..')

/** The lane whose vitest config includes a pinned test file. */
export type PinLane = 'unit' | 'e2e' | 'expected' | 'snapshot' | 'web'

export interface PinnedTest {
  file: string
  name: string
  lane: PinLane
}

export interface RegressionEntry {
  id: string
  postmortem: string
  bugClass: string
  pinnedTests: PinnedTest[]
  detectors: string[]
}

export interface RegressionCorpus {
  version: number
  bugClasses: { id: string; statement: string }[]
  entries: RegressionEntry[]
}

/** One manifest defect, reported with its corpus location. */
export interface CorpusViolation {
  location: string
  problem: string
}

const LANE_COMMANDS: Record<Exclude<PinLane, 'unit' | 'e2e'>, string> = {
  expected: 'pnpm run test:expected',
  snapshot: 'pnpm run test:snapshot',
  web: 'pnpm run test:web (builds first)',
}

/** Infer the vitest lane that owns a test path, or undefined for a foreign path. */
export function laneForFile(file: string): PinLane | undefined {
  if (file.startsWith('snapshots/')) return 'snapshot'
  if (file.endsWith('.expected.e2e.ts')) return 'expected'
  if (file.endsWith('.e2e.ts')) return file.startsWith('apps/web/') ? 'web' : 'e2e'
  if (file.endsWith('.spec.ts')) return 'unit'
  return undefined
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** Validate one pinned test against the manifest schema. */
function validatePinnedTest(test: unknown, location: string, problems: string[]): void {
  if (!isRecord(test)) {
    problems.push(`${location}: pinned test entry must be an object`)
    return
  }
  const { file, name, lane } = test
  if (typeof file !== 'string' || file.length === 0) problems.push(`${location}: pinned test needs a "file"`)
  if (typeof name !== 'string' || name.length === 0) problems.push(`${location}: pinned test needs a "name"`)
  const lanes: PinLane[] = ['unit', 'e2e', 'expected', 'snapshot', 'web']
  if (typeof lane !== 'string' || !lanes.includes(lane as PinLane)) {
    problems.push(`${location}: pinned test "lane" must be one of ${lanes.join(', ')}`)
  }
}

/** Validate the manifest's shape without touching the filesystem. */
export function validateCorpusSchema(value: unknown): { corpus: RegressionCorpus; problems: string[] } {
  const problems: string[] = []
  if (!isRecord(value)) return { corpus: { version: 0, bugClasses: [], entries: [] }, problems: ['manifest must be a JSON object'] }
  if (value.version !== 1) problems.push('version must be 1')
  const bugClasses: { id: string; statement: string }[] = []
  if (Array.isArray(value.bugClasses)) {
    for (const [index, entry] of value.bugClasses.entries()) {
      const location = `bugClasses[${index}]`
      if (!isRecord(entry) || typeof entry.id !== 'string' || entry.id.length === 0 || typeof entry.statement !== 'string' || entry.statement.length === 0) {
        problems.push(`${location}: each bug class needs a non-empty "id" and "statement"`)
        continue
      }
      bugClasses.push({ id: entry.id, statement: entry.statement })
    }
  } else {
    problems.push('bugClasses must be an array')
  }
  const classIds = new Set(bugClasses.map(entry => entry.id))
  const entries: RegressionEntry[] = []
  if (Array.isArray(value.entries)) {
    for (const [index, raw] of value.entries.entries()) {
      const location = `entries[${index}]`
      if (!isRecord(raw)) {
        problems.push(`${location}: entry must be an object`)
        continue
      }
      const { id, postmortem, bugClass, pinnedTests, detectors } = raw
      if (typeof id !== 'string' || id.length === 0) problems.push(`${location}: entry needs a non-empty "id"`)
      if (typeof postmortem !== 'string' || postmortem.length === 0) problems.push(`${location}: entry needs a "postmortem" path`)
      if (typeof bugClass !== 'string' || !classIds.has(bugClass)) problems.push(`${location}: "bugClass" must name a declared bug class`)
      const pins: PinnedTest[] = []
      if (Array.isArray(pinnedTests) && pinnedTests.length > 0) {
        for (const [pinIndex, pin] of pinnedTests.entries()) {
          const pinProblems: string[] = []
          validatePinnedTest(pin, `${location}.pinnedTests[${pinIndex}]`, pinProblems)
          problems.push(...pinProblems)
          if (pinProblems.length === 0 && isRecord(pin)) pins.push(pin as unknown as PinnedTest)
        }
      } else {
        problems.push(`${location}: entry needs a non-empty "pinnedTests" array — an incident without a permanently running test is not closed`)
      }
      const detectorPaths: string[] = []
      if (detectors !== undefined) {
        if (!Array.isArray(detectors)) problems.push(`${location}: "detectors" must be an array of script paths`)
        else for (const detector of detectors) {
          if (typeof detector !== 'string' || detector.length === 0) problems.push(`${location}: each detector must be a non-empty script path`)
          else detectorPaths.push(detector)
        }
      }
      entries.push({
        id: typeof id === 'string' ? id : '',
        postmortem: typeof postmortem === 'string' ? postmortem : '',
        bugClass: typeof bugClass === 'string' ? bugClass : '',
        pinnedTests: pins,
        detectors: detectorPaths,
      })
    }
  } else {
    problems.push('entries must be an array')
  }
  const seen = new Set<string>()
  for (const entry of entries) {
    if (seen.has(entry.id)) problems.push(`entries: duplicate entry id "${entry.id}"`)
    seen.add(entry.id)
  }
  return { corpus: { version: 1, bugClasses, entries }, problems }
}

/** The postmortem narrative files (English side), sorted by id. */
export function postmortemFiles(): string[] {
  const dir = join(root, 'docs/postmortem')
  return readdirSync(dir)
    .filter(name => /^\d{4}-.+\.md$/.test(name) && !name.endsWith('.zh.md'))
    .map(name => `docs/postmortem/${name}`)
    .sort()
}

/** Whether a test file declares the named test (`it('…')` / `test('…')`). */
export function fileDeclaresTest(filePath: string, testName: string): boolean {
  const pattern = new RegExp(
    `\\b(?:it|test)\\(\\s*['"\`]${testName.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}['"\`]`,
  )
  return pattern.test(readFileSync(join(root, filePath), 'utf8'))
}

/** Validate the corpus against the tree: files, lanes, test names, completeness. */
export function validateCorpusTree(corpus: RegressionCorpus): CorpusViolation[] {
  const violations: CorpusViolation[] = []
  const coveredPostmortems = new Set<string>()
  for (const entry of corpus.entries) {
    const location = `entries[id ${entry.id}]`
    if (entry.postmortem !== '') {
      const postmortemPath = join(root, entry.postmortem)
      if (!existsSync(postmortemPath)) {
        violations.push({ location, problem: `postmortem "${entry.postmortem}" does not exist` })
      } else {
        coveredPostmortems.add(entry.postmortem)
      }
    }
    for (const pin of entry.pinnedTests) {
      const filePath = join(root, pin.file)
      if (!existsSync(filePath)) {
        violations.push({ location, problem: `pinned test file "${pin.file}" does not exist` })
        continue
      }
      const derived = laneForFile(pin.file)
      if (derived !== pin.lane) {
        violations.push({ location, problem: `pinned test "${pin.file}" declares lane "${pin.lane}" but its path belongs to "${derived ?? 'no'}" lane` })
      }
      if (!fileDeclaresTest(pin.file, pin.name)) {
        violations.push({ location, problem: `pinned test "${pin.name}" is not declared in "${pin.file}" (renamed or removed — repin the incident)` })
      }
    }
    for (const detector of entry.detectors) {
      if (!existsSync(join(root, detector))) {
        violations.push({ location, problem: `detector "${detector}" does not exist` })
      }
    }
  }
  for (const file of postmortemFiles()) {
    if (!coveredPostmortems.has(file)) {
      violations.push({
        location: 'entries',
        problem: `postmortem "${file}" has no corpus entry — add one whose pinnedTests fail when this bug class reintroduces`,
      })
    }
  }
  return violations
}

/** Execute one pin in its vitest lane; returns the command outcome. */
function runPin(pin: PinnedTest): { status: 'passed' | 'failed' | 'skipped'; detail: string } {
  if (pin.lane !== 'unit' && pin.lane !== 'e2e') {
    return { status: 'skipped', detail: `${pin.lane} lane — run via ${LANE_COMMANDS[pin.lane]}` }
  }
  const config = pin.lane === 'e2e' ? 'vitest.e2e.config.ts' : 'vitest.config.ts'
  const result = spawnSync(
    'pnpm',
    ['exec', 'vitest', 'run', '--config', config, pin.file, '-t', pin.name],
    { cwd: root, encoding: 'utf8', timeout: 600_000 },
  )
  if (result.status === 0) return { status: 'passed', detail: pin.name }
  return { status: 'failed', detail: `${pin.name} (exit ${result.status ?? 'signal'}):\n${result.stdout}${result.stderr}` }
}

function main(): void {
  const runMode = process.argv.includes('--run')
  const manifestPath = join(root, 'scripts/regression-corpus.manifest.json')
  const { corpus, problems } = validateCorpusSchema(JSON.parse(readFileSync(manifestPath, 'utf8')))
  const violations: CorpusViolation[] = problems.map(problem => ({ location: 'schema', problem }))
  violations.push(...validateCorpusTree(corpus))
  if (violations.length > 0) {
    console.error(`verify-regression-corpus: ${violations.length} violation(s):`)
    for (const violation of violations) console.error(`  ${violation.location}: ${violation.problem}`)
    process.exitCode = 1
    return
  }
  const counts = corpus.entries.reduce<Record<string, number>>((acc, entry) => {
    acc[entry.bugClass] = (acc[entry.bugClass] ?? 0) + 1
    return acc
  }, {})
  console.log(
    `verify-regression-corpus: ${corpus.entries.length} entr(ies) across ${Object.keys(counts).length} bug class(es), `
    + `${corpus.entries.reduce((sum, entry) => sum + entry.pinnedTests.length, 0)} pinned test(s), all resolve.`,
  )
  if (!runMode) return
  let failed = false
  for (const entry of corpus.entries) {
    for (const pin of entry.pinnedTests) {
      const outcome = runPin(pin)
      if (outcome.status === 'passed') console.log(`  [${entry.id}] PASS ${pin.file} — ${outcome.detail}`)
      if (outcome.status === 'skipped') console.log(`  [${entry.id}] SKIP ${pin.file} — ${outcome.detail}`)
      if (outcome.status === 'failed') {
        failed = true
        console.error(`  [${entry.id}] FAIL ${pin.file} — ${outcome.detail}`)
      }
    }
  }
  if (failed) process.exitCode = 1
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) main()
