import type { ReactNode } from 'react'
import { NavLink, useLocation, useNavigate } from 'react-router-dom'
import {
  Button,
  Masthead,
  MastheadBrand,
  MastheadContent,
  MastheadMain,
  MastheadLogo,
  Nav,
  NavGroup,
  NavItem,
  NavList,
  Page,
  PageSidebar,
  PageSidebarBody,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core'
import { useSession } from './session'

export function Layout({ children }: { children: ReactNode }) {
  const { me, logout } = useSession()
  const location = useLocation()
  const navigate = useNavigate()

  const item = (to: string, label: string, prefix = false) => (
    <NavItem key={to} isActive={prefix ? location.pathname === to || location.pathname.startsWith(to + '/') : location.pathname === to}>
      <NavLink to={to}>{label}</NavLink>
    </NavItem>
  )

  const masthead = (
    <Masthead>
      <MastheadMain>
        <MastheadBrand>
          <MastheadLogo component="span" style={{ fontWeight: 600, fontSize: '1.1rem' }}>
            KubeVirt UI
          </MastheadLogo>
        </MastheadBrand>
      </MastheadMain>
      <MastheadContent>
        <Toolbar isFullHeight>
          <ToolbarContent>
            <ToolbarItem align={{ default: 'alignEnd' }}>{me?.user.username}</ToolbarItem>
            <ToolbarItem>
              <Button
                variant="secondary"
                onClick={async () => {
                  await logout()
                  navigate('/login')
                }}
              >
                Log out
              </Button>
            </ToolbarItem>
          </ToolbarContent>
        </Toolbar>
      </MastheadContent>
    </Masthead>
  )

  const sidebar = (
    <PageSidebar>
      <PageSidebarBody>
        <Nav>
          <NavList>{item('/', 'Overview')}</NavList>
          {me && me.memberships.length > 0 && (
            <NavGroup title="Namespaces">
              {me.memberships.map((m) => item(`/ns/${encodeURIComponent(m.namespace)}`, m.namespace, true))}
            </NavGroup>
          )}
          {me?.user.isAdmin && (
            <NavGroup title="Administration">
              {item('/admin/invites', 'Invites')}
              {item('/admin/users', 'Users')}
              {item('/admin/audit', 'Audit log')}
            </NavGroup>
          )}
        </Nav>
      </PageSidebarBody>
    </PageSidebar>
  )

  return (
    <Page masthead={masthead} sidebar={sidebar}>
      {children}
    </Page>
  )
}
