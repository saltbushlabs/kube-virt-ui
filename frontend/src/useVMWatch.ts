import { useEffect, useState } from 'react'
import { vmPath, type VM } from './api'

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
