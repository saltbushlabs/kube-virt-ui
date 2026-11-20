import { useState } from 'react'
import { Alert, Button, Flex, FlexItem, FormSelect, FormSelectOption, Label, PageSection, Title } from '@patternfly/react-core'
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table'
import { api, roles, type Membership, type Role, type UserWithMemberships } from '../../api'
import { useSession } from '../../session'
import { useApi } from '../../useApi'

function MembershipItem({
  user,
  membership,
  onChangeRole,
  onRemove,
}: {
  user: UserWithMemberships
  membership: Membership
  onChangeRole: (role: Role) => Promise<boolean>
  onRemove: () => Promise<boolean>
}) {
  const [busy, setBusy] = useState(false)

  async function run(fn: () => Promise<boolean>) {
    setBusy(true)
    try {
      await fn()
    } finally {
      setBusy(false)
    }
  }

  return (
    <Flex spaceItems={{ default: 'spaceItemsXs' }} alignItems={{ default: 'alignItemsCenter' }} flexWrap={{ default: 'nowrap' }}>
      <FlexItem>
        <Label>{membership.namespace}</Label>
      </FlexItem>
      <FlexItem>
        <FormSelect
          aria-label={`Role for ${user.username} in ${membership.namespace}`}
          value={membership.role}
          isDisabled={busy}
          style={{ width: 'auto' }}
          onChange={(_, v) => {
            const nextRole = v as Role
            if (nextRole !== membership.role) void run(() => onChangeRole(nextRole))
          }}
        >
          {roles.map((r) => (
            <FormSelectOption key={r} value={r} label={r} />
          ))}
        </FormSelect>
      </FlexItem>
      <FlexItem>
        <Button
          variant="link"
          isDanger
          isInline
          isDisabled={busy}
          aria-label={`Remove ${user.username} from ${membership.namespace}`}
          onClick={() => {
            if (confirm(`Remove ${user.username} from ${membership.namespace}? Their access is revoked immediately.`)) {
              void run(onRemove)
            }
          }}
        >
          Remove
        </Button>
      </FlexItem>
    </Flex>
  )
}

function AddMembership({
  user,
  namespaces,
  onAdd,
}: {
  user: UserWithMemberships
  namespaces: string[]
  onAdd: (namespace: string, role: Role) => Promise<boolean>
}) {
  const available = namespaces.filter((ns) => !user.memberships.some((m) => m.namespace === ns))
  const [namespace, setNamespace] = useState('')
  const [role, setRole] = useState<Role>('viewer')
  const [busy, setBusy] = useState(false)
  if (available.length === 0) return null
  const selected = available.includes(namespace) ? namespace : ''

  async function submit() {
    if (!selected || busy) return
    setBusy(true)
    try {
      const ok = await onAdd(selected, role)
      if (ok) setNamespace('')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Flex spaceItems={{ default: 'spaceItemsSm' }} flexWrap={{ default: 'nowrap' }}>
      <FlexItem>
        <FormSelect aria-label="Namespace" value={selected} isDisabled={busy} onChange={(_, v) => setNamespace(v)}>
          <FormSelectOption value="" label="Add namespace…" />
          {available.map((ns) => (
            <FormSelectOption key={ns} value={ns} label={ns} />
          ))}
        </FormSelect>
      </FlexItem>
      <FlexItem>
        <FormSelect aria-label="Role" value={role} isDisabled={busy} onChange={(_, v) => setRole(v as Role)}>
          {roles.map((r) => (
            <FormSelectOption key={r} value={r} label={r} />
          ))}
        </FormSelect>
      </FlexItem>
      <FlexItem>
        <Button variant="secondary" isLoading={busy} isDisabled={!selected || busy} onClick={() => void submit()}>
          Add
        </Button>
      </FlexItem>
    </Flex>
  )
}

export function UsersPage() {
  const { me, refresh } = useSession()
  const users = useApi<UserWithMemberships[]>('/admin/users')
  const namespaces = useApi<string[]>('/admin/namespaces')
  const [error, setError] = useState('')

  async function act(fn: () => Promise<unknown>, refreshSession = false): Promise<boolean> {
    setError('')
    let ok = false
    try {
      await fn()
      ok = true
    } catch (e) {
      setError((e as Error).message)
    }
    await users.reload()
    if (ok && refreshSession) {
      try {
        await refresh()
      } catch (e) {
        setError((prev) => prev || (e as Error).message)
      }
    }
    return ok
  }

  return (
    <>
      <PageSection>
        <Title headingLevel="h1">Users</Title>
      </PageSection>
      <PageSection>
        {(error || users.error || namespaces.error) && <Alert variant="danger" isInline title={error || users.error || namespaces.error} />}
        <Table aria-label="Users" variant="compact">
          <Thead>
            <Tr>
              <Th>Username</Th>
              <Th>Email</Th>
              <Th>Namespaces</Th>
              <Th>Status</Th>
              <Th screenReaderText="Actions" />
            </Tr>
          </Thead>
          <Tbody>
            {(users.data ?? []).map((u) => {
              const isSelf = u.id === me?.user.id
              return (
                <Tr key={u.id}>
                  <Td dataLabel="Username">
                    {u.username} {u.isAdmin && <Label color="purple">admin</Label>}
                  </Td>
                  <Td dataLabel="Email">{u.email || '—'}</Td>
                  <Td dataLabel="Namespaces">
                    <Flex direction={{ default: 'column' }} spaceItems={{ default: 'spaceItemsSm' }}>
                      {u.memberships.length > 0 && (
                        <Flex direction={{ default: 'column' }} spaceItems={{ default: 'spaceItemsXs' }}>
                          {u.memberships.map((m) => (
                            <MembershipItem
                              key={m.namespace}
                              user={u}
                              membership={m}
                              onChangeRole={(role) =>
                                act(() => api('PUT', `/admin/users/${u.id}/memberships/${encodeURIComponent(m.namespace)}`, { role }), isSelf)
                              }
                              onRemove={() =>
                                act(() => api('DELETE', `/admin/users/${u.id}/memberships/${encodeURIComponent(m.namespace)}`), isSelf)
                              }
                            />
                          ))}
                        </Flex>
                      )}
                      <AddMembership
                        user={u}
                        namespaces={namespaces.data ?? []}
                        onAdd={(ns, role) =>
                          act(() => api('PUT', `/admin/users/${u.id}/memberships/${encodeURIComponent(ns)}`, { role }), isSelf)
                        }
                      />
                    </Flex>
                  </Td>
                  <Td dataLabel="Status">{u.disabled ? <Label color="red">disabled</Label> : <Label color="green">active</Label>}</Td>
                  <Td isActionCell>
                    {!isSelf && (
                      <Button variant="link" isDanger={!u.disabled} onClick={() => void act(() => api('PATCH', `/admin/users/${u.id}`, { disabled: !u.disabled }))}>
                        {u.disabled ? 'Enable' : 'Disable'}
                      </Button>
                    )}
                  </Td>
                </Tr>
              )
            })}
          </Tbody>
        </Table>
      </PageSection>
    </>
  )
}
