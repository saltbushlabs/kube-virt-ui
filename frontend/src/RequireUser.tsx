import { useState, type ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { Alert, Bullseye, Button, EmptyState, EmptyStateActions, EmptyStateBody, EmptyStateFooter, Spinner } from '@patternfly/react-core'
import { useSession } from './session'
import { Layout } from './Layout'

export function RequireUser({ admin, children }: { admin?: boolean; children: ReactNode }) {
  const { me, loading, error, refresh } = useSession()
  const [retrying, setRetrying] = useState(false)
  const location = useLocation()
  if (loading)
    return (
      <Bullseye>
        <Spinner />
      </Bullseye>
    )
  if (!me && error)
    return (
      <Bullseye style={{ minHeight: '100vh', padding: 16 }}>
        <EmptyState titleText="Unable to verify session" headingLevel="h1">
          <EmptyStateBody>
            <Alert variant="danger" isInline title={error} />
          </EmptyStateBody>
          <EmptyStateFooter>
            <EmptyStateActions>
              <Button
                isLoading={retrying}
                isDisabled={retrying}
                onClick={async () => {
                  setRetrying(true)
                  try {
                    await refresh()
                  } catch {
                    // Error state is tracked by SessionProvider.
                  } finally {
                    setRetrying(false)
                  }
                }}
              >
                Retry
              </Button>
            </EmptyStateActions>
          </EmptyStateFooter>
        </EmptyState>
      </Bullseye>
    )
  if (!me) return <Navigate to={`/login?next=${encodeURIComponent(location.pathname + location.search + location.hash)}`} replace />
  if (admin && !me.user.isAdmin) return <Navigate to="/" replace />
  return <Layout>{children}</Layout>
}
