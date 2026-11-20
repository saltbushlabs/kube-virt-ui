import { describe, expect, it } from 'vitest'
import type { VM } from './api'
import { diskNote, vmSize } from './vmHelpers'

function makeVM(overrides: Partial<VM> = {}): VM {
  return {
    name: 'web-1',
    namespace: 'demo',
    status: 'Running',
    ready: true,
    runStrategy: 'Always',
    ips: [],
    created: '2026-01-01T00:00:00Z',
    disks: [],
    ...overrides,
  }
}

describe('diskNote', () => {
  it('reports problems with highest priority', () => {
    expect(
      diskNote({
        name: 'root',
        phase: 'ImportInProgress',
        progress: '10%',
        problem: 'exceeded quota',
      }),
    ).toEqual({ text: 'root: exceeded quota', problem: true })
  })

  it('explains WaitForFirstConsumer disks', () => {
    expect(diskNote({ name: 'root', phase: 'WaitForFirstConsumer' })).toEqual({
      text: 'root: prepared on first start',
      problem: false,
    })
  })

  it('shows non-Succeeded phase with optional progress', () => {
    expect(diskNote({ name: 'root', phase: 'ImportInProgress', progress: '42%' })).toEqual({
      text: 'root: ImportInProgress 42%',
      problem: false,
    })
    expect(diskNote({ name: 'data', phase: 'Pending' })).toEqual({
      text: 'data: Pending',
      problem: false,
    })
  })

  it('returns null when succeeded or phase is empty', () => {
    expect(diskNote({ name: 'root', phase: 'Succeeded' })).toBeNull()
    expect(diskNote({ name: 'root', phase: '' })).toBeNull()
  })
})

describe('vmSize', () => {
  it('returns instancetype when present', () => {
    expect(vmSize(makeVM({ instancetype: 'u1.medium', cpus: 2, memory: '4Gi' }))).toBe('u1.medium')
  })

  it('formats custom CPU and memory combinations', () => {
    expect(vmSize(makeVM({ cpus: 4, memory: '8Gi' }))).toBe('4 vCPU, 8Gi')
    expect(vmSize(makeVM({ cpus: 2 }))).toBe('2 vCPU')
    expect(vmSize(makeVM({ memory: '4Gi' }))).toBe('4Gi')
  })

  it('returns an em dash when no size details are set', () => {
    expect(vmSize(makeVM())).toBe('—')
  })
})
