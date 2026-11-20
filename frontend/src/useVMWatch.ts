import { useEffect, useState } from 'react'
import { vmPath, type VM } from './api'
import { useSession } from './session'

// useVMWatch subscribes to the namespace's live VM list over server-sent
// events. EventSource reconnects by itself after the server's periodic
// stream cut; a refused reconnect (e.g. session expiry or membership
// removed) closes it, clears stale VM data, and re-checks the session.
export function useVMWatch(ns: string) {
  const { refresh } = useSession()
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
    let closed = false
    const es = new EventSource('/api' + vmPath(ns) + '/watch/vms')
    es.onmessage = (e) => {
      setVms(JSON.parse(e.data))
      setError('')
    }
    es.addEventListener('failure', (e) => setError(JSON.parse((e as MessageEvent).data)))
    es.onerror = () => {
      if (closed || es.readyState !== EventSource.CLOSED) return
      setVms(null)
      setError('Live updates stopped. Reload the page to retry.')
      void refresh().catch(() => {})
    }
    return () => {
      closed = true
      es.close()
    }
  }, [ns, refresh])

  return { vms, error }
}
