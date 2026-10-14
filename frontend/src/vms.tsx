import { useEffect, useState } from 'react'
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
import { api, availableActions, vmPath, type Disk, type PowerAction, type Role, type VM } from './api'

// useVMWatch subscribes to the namespace's live VM list over server-sent
// events. EventSource reconnects by itself after the server's periodic
// stream cut; a refused reconnect (e.g. membership removed) closes it.
export function useVMWatch(ns: string) {
  const [vms, setVms] = useState<VM[] | null>(null)
  const [error, setError] = useState('')

  // Clear the previous namespace's list during render when ns changes, so
  // it is not shown (or acted on) under the new namespace's title while
  // its stream connects.
  const [prevNs, setPrevNs] = useState(ns)
  if (prevNs !== ns) {
    setPrevNs(ns)
    setVms(null)
    setError('')
  }

  useEffect(() => {
    const es = new EventSource('/api' + vmPath(ns) + '/watch/vms')
    es.onmessage = (e) => {
      setVms(JSON.parse(e.data))
      setError('')
    }
    es.addEventListener('failure', (e) => setError(JSON.parse((e as MessageEvent).data)))
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) setError('Live updates stopped. Reload the page to retry.')
    }
    return () => es.close()
  }, [ns])

  return { vms, error }
}

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

// diskNote explains a disk that is not ready yet: a CDI problem, or import
// progress.
export function diskNote(d: Disk): { text: string; problem: boolean } | null {
  if (d.problem) return { text: `${d.name}: ${d.problem}`, problem: true }
  // Storage that binds on first use prepares the disk when the VM first starts.
  if (d.phase === 'WaitForFirstConsumer') return { text: `${d.name}: prepared on first start`, problem: false }
  if (d.phase && d.phase !== 'Succeeded') return { text: `${d.name}: ${d.phase}${d.progress ? ' ' + d.progress : ''}`, problem: false }
  return null
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

export function vmSize(vm: VM): string {
  if (vm.instancetype) return vm.instancetype
  return [vm.cpus ? `${vm.cpus} vCPU` : '', vm.memory ?? ''].filter(Boolean).join(', ') || '—'
}
