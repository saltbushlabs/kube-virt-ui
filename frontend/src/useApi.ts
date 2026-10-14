import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from './api'

// useApi GETs path and exposes the result with a reload function. When the
// path changes the previous result is cleared and any in-flight request is
// aborted, so one namespace's data is never shown under another's title and
// a slow earlier response cannot overwrite a newer one.
export function useApi<T>(path: string) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const abortRef = useRef<AbortController | null>(null)

  // Reset during render when the path changes (React's recommended
  // alternative to an effect) so stale data never renders for the new path.
  const [prevPath, setPrevPath] = useState(path)
  if (prevPath !== path) {
    setPrevPath(path)
    setData(null)
    setError('')
  }

  const reload = useCallback(async () => {
    abortRef.current?.abort()
    const ac = new AbortController()
    abortRef.current = ac
    try {
      const result = await api<T>('GET', path, undefined, ac.signal)
      if (!ac.signal.aborted) {
        setData(result)
        setError('')
      }
    } catch (e) {
      if (!ac.signal.aborted) setError((e as Error).message)
    }
  }, [path])

  useEffect(() => {
    reload()
    return () => abortRef.current?.abort()
  }, [reload])

  const clearError = useCallback(() => setError(''), [])

  return { data, error, reload, clearError }
}
