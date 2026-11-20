import { describe, expect, it } from 'vitest'
import { availableActions, inviteStatus, vmPath, type Invite, type VM } from './api'

function makeVM(status: string): VM {
  return {
    name: 'web-1',
    namespace: 'demo',
    status,
    ready: status === 'Running',
    runStrategy: 'Always',
    ips: [],
    created: '2026-01-01T00:00:00Z',
    disks: [],
  }
}

function makeInvite(overrides: Partial<Invite> = {}): Invite {
  return {
    id: 1,
    email: 'user@example.com',
    namespace: 'demo',
    role: 'operator',
    makeAdmin: false,
    createdAt: '2026-01-01T00:00:00Z',
    expiresAt: '2099-01-01T00:00:00Z',
    usedAt: null,
    revokedAt: null,
    ...overrides,
  }
}

describe('availableActions', () => {
  it('returns no actions for undefined or viewer roles', () => {
    expect(availableActions(makeVM('Running'), undefined)).toEqual([])
    expect(availableActions(makeVM('Stopped'), 'viewer')).toEqual([])
    expect(availableActions(makeVM('Running'), 'viewer')).toEqual([])
    expect(availableActions(makeVM('Paused'), 'viewer')).toEqual([])
  })

  it('returns state-appropriate actions for operator and owner roles', () => {
    for (const role of ['operator', 'owner'] as const) {
      expect(availableActions(makeVM('Stopped'), role)).toEqual(['start'])
      expect(availableActions(makeVM('Stopping'), role)).toEqual([])
      expect(availableActions(makeVM('Running'), role)).toEqual(['stop', 'restart', 'pause'])
      expect(availableActions(makeVM('Paused'), role)).toEqual(['unpause', 'stop'])
      expect(availableActions(makeVM('Starting'), role)).toEqual(['stop'])
      expect(availableActions(makeVM('Provisioning'), role)).toEqual(['stop'])
      expect(availableActions(makeVM('Migrating'), role)).toEqual(['stop'])
      expect(availableActions(makeVM('CrashLoopBackOff'), role)).toEqual(['stop'])
    }
  })
})

describe('inviteStatus', () => {
  it('prioritizes used over revoked and expired', () => {
    expect(
      inviteStatus(
        makeInvite({
          usedAt: '2026-01-02T00:00:00Z',
          revokedAt: '2026-01-03T00:00:00Z',
          expiresAt: '2020-01-01T00:00:00Z',
        }),
      ),
    ).toBe('used')
  })

  it('prioritizes revoked over expired', () => {
    expect(
      inviteStatus(
        makeInvite({
          revokedAt: '2026-01-03T00:00:00Z',
          expiresAt: '2020-01-01T00:00:00Z',
        }),
      ),
    ).toBe('revoked')
  })

  it('returns expired for past expiresAt and pending for future expiresAt', () => {
    expect(inviteStatus(makeInvite({ expiresAt: '2020-01-01T00:00:00Z' }))).toBe('expired')
    expect(inviteStatus(makeInvite({ expiresAt: '2099-01-01T00:00:00Z' }))).toBe('pending')
  })
})

describe('vmPath', () => {
  it('encodes namespace and optional VM name', () => {
    expect(vmPath('team-a')).toBe('/namespaces/team-a')
    expect(vmPath('team a', 'vm/1')).toBe('/namespaces/team%20a/vms/vm%2F1')
  })
})
