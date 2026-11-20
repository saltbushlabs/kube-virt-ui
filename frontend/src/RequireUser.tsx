import type { ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { Bullseye, Spinner } from '@patternfly/react-core'
import { useSession } from './session'
import { Layout } from './Layout'

export function RequireUser({ admin, children }: { admin?: boolean; children: ReactNode }) {
  const { me, loading } = useSession()
  const location = useLocation()
  if (loading)
    return (
      <Bullseye>
        <Spinner />
      </Bullseye>
    )
  if (!me) return <Navigate to={`/login?next=${encodeURIComponent(location.pathname + location.search + location.hash)}`} replace />
  if (admin && !me.user.isAdmin) return <Navigate to="/" replace />
  return <Layout>{children}</Layout>
}
