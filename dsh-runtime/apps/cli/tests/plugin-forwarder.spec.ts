import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { runPlugin } from '../src/plugin.ts'

const profile = 'plugin-forwarder-under-test'

describe('dsh plugin forwarder', () => {
  const savedHome = process.env.DSH_HOME
  const savedPath = process.env.PATH
  const homes: string[] = []

  afterEach(() => {
    process.env.DSH_HOME = savedHome
    process.env.PATH = savedPath
    for (const home of homes.splice(0)) rmSync(home, { recursive: true, force: true })
  })

  function useTempHome(): string {
    const home = mkdtempSync(join(tmpdir(), 'dsh-plugin-'))
    homes.push(home)
    process.env.DSH_HOME = home
    return home
  }

  it('reports a missing pnpm as exit 127 with the PATH diagnostic', () => {
    const home = useTempHome()
    process.env.PATH = ''
    const statuses: string[] = []
    const writes = process.stderr.write.bind(process.stderr)
    process.stderr.write = (chunk: Uint8Array | string): boolean => {
      statuses.push(typeof chunk === 'string' ? chunk : Buffer.from(chunk).toString('utf8'))
      return true
    }
    let code: number
    try {
      code = runPlugin(profile, ['--version'])
    } finally {
      process.stderr.write = writes
    }
    expect(code).toBe(127)
    expect(statuses.join('')).toContain('dsh: pnpm not found on PATH — install pnpm to manage profile plugins')
    // The profile was initialized before the forwarding failed; a later
    // invocation with pnpm installed starts from a materialized profile.
    expect(existsSync(join(home, 'profiles', profile, 'package.json'))).toBe(true)
  })

  it('initializes a non-template profile before forwarding and says so on stderr', () => {
    const home = useTempHome()
    const statuses: string[] = []
    const writes = process.stderr.write.bind(process.stderr)
    process.stderr.write = (chunk: Uint8Array | string): boolean => {
      statuses.push(typeof chunk === 'string' ? chunk : Buffer.from(chunk).toString('utf8'))
      return true
    }
    try {
      runPlugin(profile, ['--version'])
    } finally {
      process.stderr.write = writes
    }
    expect(statuses.join('')).toContain(`dsh: initialized profile ${profile} at ${join(home, 'profiles', profile)}`)
    const manifest = JSON.parse(readFileSync(join(home, 'profiles', profile, 'package.json'), 'utf8')) as {
      dsh?: { profile?: { bundles?: string[]; patchReload?: string } }
    }
    expect(manifest.dsh?.profile?.bundles).toEqual(['@deepseek-ai/dsh-base'])
  })
})
