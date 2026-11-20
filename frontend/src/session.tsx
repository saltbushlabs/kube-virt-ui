import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { api, ApiError, type Me } from './api'

interface Session {
  me: Me | null
  loading: boolean
  error: string | null
  refresh: () => Promise<void>
  logout: () => Promise<void>
}

const SessionContext = createContext<Session | null>(null)

export function SessionProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null)
  const meRef = useRef<Me | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      const next = await api<Me>('GET', '/me')
      meRef.current = next
      setMe(next)
      setError(null)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        meRef.current = null
        setMe(null)
        setError(null)
        return
      }
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
    void refresh().catch(() => {})
  }, [refresh])

  return <SessionContext.Provider value={{ me, loading, error, refresh, logout }}>{children}</SessionContext.Provider>
}

export function useSession(): Session {
  const s = useContext(SessionContext)
  if (!s) throw new Error('useSession outside SessionProvider')
  return s
}
