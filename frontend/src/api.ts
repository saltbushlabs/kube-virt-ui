export type Role = 'owner' | 'operator' | 'viewer'
export const roles: Role[] = ['owner', 'operator', 'viewer']

export interface User {
  id: number
  username: string
  email: string
  isAdmin: boolean
  disabled: boolean
  createdAt: string
}

export interface Membership {
  userId: number
  namespace: string
  role: Role
}

export interface Me {
  user: User
  memberships: Membership[]
}

export interface InviteInfo {
  email: string
  namespace: string
  role: Role | ''
  makeAdmin: boolean
  expiresAt: string
}

export interface Invite extends InviteInfo {
  id: number
  createdAt: string
  usedAt: string | null
  revokedAt: string | null
}

export interface UserWithMemberships extends User {
  memberships: Membership[]
}

export interface AuditEntry {
  at: string
  actor: string
  action: string
  namespace: string
  target: string
  ip: string
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export async function api<T = void>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const res = await fetch('/api' + path, {
    method,
    signal,
    credentials: 'same-origin',
    headers: {
      // Required by the backend's CSRF check on state-changing requests.
      'X-Requested-With': 'kvui',
      ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    const data = await res.json().catch(() => ({}))
    throw new ApiError(res.status, data.error ?? res.statusText)
  }
  if (res.status === 204) return undefined as T
  return res.json()
}

export function inviteStatus(i: Invite): 'used' | 'revoked' | 'expired' | 'pending' {
  if (i.usedAt) return 'used'
  if (i.revokedAt) return 'revoked'
  if (new Date(i.expiresAt) <= new Date()) return 'expired'
  return 'pending'
}

export interface VM {
  name: string
  namespace: string
  status: string
  ready: boolean
  runStrategy: string
  instancetype?: string
  cpus?: number
  memory?: string
  node?: string
  ips: string[]
  os?: string
  created: string
  disks: Disk[]
}

export interface Disk {
  name: string
  phase: string
  progress?: string
  problem?: string
}

export type PowerAction = 'start' | 'stop' | 'restart' | 'pause' | 'unpause'

// Which power actions make sense for a VM in its current state. The
// apiserver still has the final say on whether the user may perform them.
export function availableActions(vm: VM, role: Role | undefined): PowerAction[] {
  if (!role || role === 'viewer') return []
  switch (vm.status) {
    case 'Stopped':
      return ['start']
    case 'Stopping':
      return []
    case 'Running':
      return ['stop', 'restart', 'pause']
    case 'Paused':
      return ['unpause', 'stop']
    default:
      // Starting, Provisioning, Migrating and error states can still be stopped.
      return ['stop']
  }
}

export function vmPath(ns: string, name?: string): string {
  const base = `/namespaces/${encodeURIComponent(ns)}`
  return name === undefined ? base : `${base}/vms/${encodeURIComponent(name)}`
}

export interface KindRef {
  name: string
  kind: string
}

export interface Instancetype extends KindRef {
  cpus: number
  memory: string
  description?: string
}

export interface Preference extends KindRef {
  displayName?: string
}

export interface VMOptions {
  instancetypes: Instancetype[]
  preferences: Preference[]
  storageClasses: { name: string; default: boolean }[]
}

export type DiskSource = 'registry' | 'http' | 'containerdisk' | 'blank' | 'image'

export interface CreateVMRequest {
  name: string
  instancetype?: KindRef
  cpus?: number
  memory?: string
  preference?: KindRef
  diskSource: DiskSource
  diskUrl?: string
  image?: string
  cdrom?: string
  diskSize?: string
  storageClass?: string
  sshKeys?: string[]
  userData?: string
  start: boolean
}

export interface Image {
  name: string
  type: 'iso' | 'disk'
  size: string
  phase: string
  progress?: string
  problem?: string
  created: string
}
