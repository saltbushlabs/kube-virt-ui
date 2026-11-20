import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
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
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      const next = await api<Me>('GET', '/me')
      setMe(next)
      setError(null)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        setMe(null)
        setError(null)
        return
      }
      setError(e instanceof Error && e.message ? e.message : 'Failed to verify session')
      throw e
    } finally {
      setLoading(false)
    }
  }, [])

  const logout = useCallback(async () => {
    await api('POST', '/logout')
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
