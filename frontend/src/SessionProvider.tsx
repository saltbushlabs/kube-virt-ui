import { useCallback, useEffect, useState, type ReactNode } from 'react'
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
  const [loading, setLoading] = useState(true)

  const refresh = useCallback(async () => {
    try {
      setMe(await fetchMe())
    } finally {
      setLoading(false)
    }
  }, [])

  const logout = useCallback(async () => {
    await api('POST', '/logout')
    setMe(null)
  }, [])

  useEffect(() => {
    let cancelled = false
    fetchMe()
      .then((user) => {
        if (!cancelled) setMe(user)
      })
      .catch(() => {})
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  return <SessionContext.Provider value={{ me, loading, refresh, logout }}>{children}</SessionContext.Provider>
}
