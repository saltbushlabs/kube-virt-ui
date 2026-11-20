import type { Disk, VM } from './api'

// diskNote explains a disk that is not ready yet: a CDI problem, or import
// progress.
export function diskNote(d: Disk): { text: string; problem: boolean } | null {
  if (d.problem) return { text: `${d.name}: ${d.problem}`, problem: true }
  // Storage that binds on first use prepares the disk when the VM first starts.
  if (d.phase === 'WaitForFirstConsumer') return { text: `${d.name}: prepared on first start`, problem: false }
  if (d.phase && d.phase !== 'Succeeded') return { text: `${d.name}: ${d.phase}${d.progress ? ' ' + d.progress : ''}`, problem: false }
  return null
}

export function vmSize(vm: VM): string {
  if (vm.instancetype) return vm.instancetype
  return [vm.cpus ? `${vm.cpus} vCPU` : '', vm.memory ?? ''].filter(Boolean).join(', ') || '—'
}
