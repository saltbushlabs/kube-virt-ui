import '@patternfly/react-core/dist/styles/base.css'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { SessionProvider } from './SessionProvider'
import { RequireUser } from './RequireUser'
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
