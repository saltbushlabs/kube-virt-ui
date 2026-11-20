import { useState } from 'react'
import {
  Button,
  Content,
  Divider,
  Dropdown,
  DropdownItem,
  DropdownList,
  Label,
  MenuToggle,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  TextInput,
} from '@patternfly/react-core'
import { api, availableActions, vmPath, type PowerAction, type Role, type VM } from './api'
import { diskNote } from './vmHelpers'

const statusColors: Record<string, 'green' | 'grey' | 'orange' | 'blue' | 'red'> = {
  Running: 'green',
  Stopped: 'grey',
  Paused: 'orange',
  Starting: 'blue',
  Stopping: 'blue',
  Provisioning: 'blue',
  Migrating: 'blue',
  WaitingForVolumeBinding: 'blue',
  Terminating: 'blue',
}

export function VMStatus({ status }: { status: string }) {
  return <Label color={statusColors[status] ?? 'red'}>{status}</Label>
}

// VMStatusCell shows the status plus, while provisioning, why it is waiting.
export function VMStatusCell({ vm }: { vm: VM }) {
  const notes = vm.disks.map(diskNote).filter((n) => n !== null)
  return (
    <>
      <VMStatus status={vm.status} />
      {notes.map((n) => (
        <div key={n.text} style={{ fontSize: 'var(--pf-t--global--font--size--sm)', marginTop: 4 }}>
          {n.problem ? <span style={{ color: 'var(--pf-t--global--text--color--status--danger--default)' }}>{n.text}</span> : n.text}
        </div>
      ))}
    </>
  )
}

const labels: Record<PowerAction, string> = {
  start: 'Start',
  stop: 'Stop',
  restart: 'Restart',
  pause: 'Pause',
  unpause: 'Unpause',
}

// Stop and restart interrupt whatever the guest is doing, so confirm them.
const needsConfirm: PowerAction[] = ['stop', 'restart']

export function PowerMenu({
  vm,
  role,
  onError,
  onDeleted,
}: {
  vm: VM
  role: Role | undefined
  onError: (msg: string) => void
  onDeleted?: () => void
}) {
  const [open, setOpen] = useState(false)
  const [confirming, setConfirming] = useState<PowerAction | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [typedName, setTypedName] = useState('')
  const [busy, setBusy] = useState(false)
  const actions = availableActions(vm, role)
  const canDelete = role === 'owner'

  async function remove() {
    setBusy(true)
    try {
      await api('DELETE', vmPath(vm.namespace, vm.name))
      setDeleting(false)
      onDeleted?.()
    } catch (e) {
      onError(`Delete ${vm.name}: ${(e as Error).message}`)
      setDeleting(false)
    } finally {
      setBusy(false)
    }
  }

  async function run(action: PowerAction) {
    setBusy(true)
    try {
      await api('POST', `${vmPath(vm.namespace, vm.name)}/${action}`)
    } catch (e) {
      onError(`${labels[action]} ${vm.name}: ${(e as Error).message}`)
    } finally {
      setBusy(false)
      setConfirming(null)
    }
  }

  return (
    <>
      <Dropdown
        isOpen={open}
        onOpenChange={setOpen}
        popperProps={{ position: 'right' }}
        toggle={(ref) => (
          <MenuToggle ref={ref} variant="secondary" isDisabled={(actions.length === 0 && !canDelete) || busy} onClick={() => setOpen(!open)}>
            Actions
          </MenuToggle>
        )}
      >
        <DropdownList>
          {actions.map((a) => (
            <DropdownItem
              key={a}
              onClick={() => {
                setOpen(false)
                if (needsConfirm.includes(a)) setConfirming(a)
                else run(a)
              }}
            >
              {labels[a]}
            </DropdownItem>
          ))}
          {canDelete && actions.length > 0 && <Divider component="li" />}
          {canDelete && (
            <DropdownItem
              key="delete"
              isDanger
              onClick={() => {
                setOpen(false)
                setTypedName('')
                setDeleting(true)
              }}
            >
              Delete
            </DropdownItem>
          )}
        </DropdownList>
      </Dropdown>
      <Modal variant="small" isOpen={confirming !== null} onClose={() => setConfirming(null)}>
        <ModalHeader title={`${confirming ? labels[confirming] : ''} ${vm.name}?`} titleIconVariant="warning" />
        <ModalBody>
          The guest is asked to shut down; if it does not respond within its grace period it is powered off and unsaved
          work is lost.
        </ModalBody>
        <ModalFooter>
          <Button variant="danger" isLoading={busy} isDisabled={busy} onClick={() => confirming && run(confirming)}>
            {confirming ? labels[confirming] : ''}
          </Button>
          <Button variant="link" onClick={() => setConfirming(null)}>
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
      <Modal variant="small" isOpen={deleting} onClose={() => setDeleting(false)}>
        <ModalHeader title={`Delete ${vm.name}?`} titleIconVariant="danger" />
        <ModalBody>
          <Content component="p">
            The virtual machine and the disks created with it are permanently deleted. This cannot be undone.
          </Content>
          <Content component="p">
            Type <b>{vm.name}</b> to confirm.
          </Content>
          <TextInput aria-label="VM name" value={typedName} onChange={(_, v) => setTypedName(v)} />
        </ModalBody>
        <ModalFooter>
          <Button variant="danger" isLoading={busy} isDisabled={busy || typedName !== vm.name} onClick={remove}>
            Delete
          </Button>
          <Button variant="link" onClick={() => setDeleting(false)}>
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </>
  )
}
