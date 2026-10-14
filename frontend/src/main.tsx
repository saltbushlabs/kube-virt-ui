import '@patternfly/react-core/dist/styles/base.css'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { Bullseye, Spinner } from '@patternfly/react-core'
import { SessionProvider, useSession } from './session'
import { Layout } from './Layout'
import { LoginPage } from './pages/Login'
import { InvitePage } from './pages/Invite'
import { HomePage } from './pages/Home'
import { VMsPage } from './pages/VMs'
import { VMPage } from './pages/VM'
import { CreateVMPage } from './pages/CreateVM'
import { ImagesPage } from './pages/Images'
import { InvitesPage } from './pages/admin/Invites'
import { UsersPage } from './pages/admin/Users'
import { AuditPage } from './pages/admin/Audit'

function RequireUser({ admin, children }: { admin?: boolean; children: React.ReactNode }) {
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

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <SessionProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route path="/invite/:token" element={<InvitePage />} />
          <Route path="/" element={<RequireUser><HomePage /></RequireUser>} />
          <Route path="/ns/:ns" element={<RequireUser><VMsPage /></RequireUser>} />
          <Route path="/ns/:ns/images" element={<RequireUser><ImagesPage /></RequireUser>} />
          <Route path="/ns/:ns/create" element={<RequireUser><CreateVMPage /></RequireUser>} />
          <Route path="/ns/:ns/vms/:name" element={<RequireUser><VMPage /></RequireUser>} />
          <Route path="/admin/invites" element={<RequireUser admin><InvitesPage /></RequireUser>} />
          <Route path="/admin/users" element={<RequireUser admin><UsersPage /></RequireUser>} />
          <Route path="/admin/audit" element={<RequireUser admin><AuditPage /></RequireUser>} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </BrowserRouter>
    </SessionProvider>
  </StrictMode>,
)
