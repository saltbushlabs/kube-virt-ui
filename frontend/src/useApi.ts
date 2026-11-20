import { useCallback, useEffect, useRef, useState } from 'react'
import { api, ApiError } from './api'

async function loadPath<T>(
  path: string,
  abortRef: { current: AbortController | null },
  onData: (result: T) => void,
  onError: (msg: string) => void,
  onErrorStatus: (status: number | null) => void,
) {
  abortRef.current?.abort()
  const ac = new AbortController()
  abortRef.current = ac
  try {
    const result = await api<T>('GET', path, undefined, ac.signal)
    if (!ac.signal.aborted) {
      onData(result)
      onError('')
      onErrorStatus(null)
    }
  } catch (e) {
    if (!ac.signal.aborted) {
      onError((e as Error).message)
      onErrorStatus(e instanceof ApiError ? e.status : null)
    }
  }
}

// useApi GETs path and exposes the result with a reload function. When the
// path changes the previous result is cleared and any in-flight request is
// aborted, so one namespace's data is never shown under another's title and
// a slow earlier response cannot overwrite a newer one.
export function useApi<T>(path: string) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [errorStatus, setErrorStatus] = useState<number | null>(null)
  const abortRef = useRef<AbortController | null>(null)

  // Reset during render when the path changes (React's recommended
  // alternative to an effect) so stale data never renders for the new path.
  const [prevPath, setPrevPath] = useState(path)
  if (prevPath !== path) {
    setPrevPath(path)
    setData(null)
    setError('')
    setErrorStatus(null)
  }

  const abort = useCallback(() => abortRef.current?.abort(), [])

  const reload = useCallback(async () => {
    await loadPath(path, abortRef, setData, setError, setErrorStatus)
  }, [path])

  useEffect(() => {
    void loadPath(path, abortRef, setData, setError, setErrorStatus)
    return abort
  }, [path, abort])

  const clearError = useCallback(() => {
    setError('')
    setErrorStatus(null)
  }, [])

  return { data, error, errorStatus, reload, clearError }
}
