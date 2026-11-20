import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { api, ApiError, type Me } from './api'
import { SessionContext } from './session'

async function fetchMe(): Promise<Me | null> {
  try {
    return await api<Me>('GET', '/me')
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) throw e
    return null
  }
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null)
  const meRef = useRef<Me | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      const next = await fetchMe()
      meRef.current = next
      setMe(next)
      setError(null)
    } catch (e) {
      // Only record a session load error when no active session exists;
      // otherwise a transient background refresh failure would leave a stale
      // error in context while `me` stays populated.
      if (!meRef.current) {
        setError(e instanceof Error && e.message ? e.message : 'Failed to verify session')
      }
      throw e
    } finally {
      setLoading(false)
    }
  }, [])

  const logout = useCallback(async () => {
    await api('POST', '/logout')
    meRef.current = null
    setMe(null)
    setError(null)
  }, [])

  useEffect(() => {
    let cancelled = false
    fetchMe()
      .then((user) => {
        if (!cancelled) {
          meRef.current = user
          setMe(user)
          setError(null)
        }
      })
      .catch((e) => {
        if (!cancelled && !meRef.current) {
          setError(e instanceof Error && e.message ? e.message : 'Failed to verify session')
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  return <SessionContext.Provider value={{ me, loading, error, refresh, logout }}>{children}</SessionContext.Provider>
}
