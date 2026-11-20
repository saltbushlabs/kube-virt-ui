import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Alert, AlertActionCloseButton, Bullseye, Button, EmptyState, EmptyStateBody, Flex, FlexItem, PageSection, Spinner, Title } from '@patternfly/react-core'
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table'
import { useSession } from '../session'
import { NamespaceTabs } from '../NamespaceTabs'
import { useVMWatch } from '../useVMWatch'
import { vmSize } from '../vmHelpers'
import { PowerMenu, VMStatusCell } from '../vms'

export function VMsPage() {
  const { ns = '' } = useParams()
  const { me } = useSession()
  const role = me?.memberships.find((m) => m.namespace === ns)?.role
  const { vms, error } = useVMWatch(ns)
  const [actionError, setActionError] = useState('')
  const navigate = useNavigate()
  const create = role === 'owner' && (
    <Button onClick={() => navigate(`/ns/${encodeURIComponent(ns)}/create`)}>Create virtual machine</Button>
  )

  return (
    <>
      <PageSection>
        <Flex justifyContent={{ default: 'justifyContentSpaceBetween' }} alignItems={{ default: 'alignItemsCenter' }}>
          <FlexItem>
            <Title headingLevel="h1">{ns}</Title>
          </FlexItem>
          <FlexItem>{create}</FlexItem>
        </Flex>
        <NamespaceTabs ns={ns} />
      </PageSection>
      <PageSection>
        {error && <Alert variant="danger" isInline title={error} />}
        {actionError && (
          <Alert variant="danger" isInline title={actionError} actionClose={<AlertActionCloseButton onClose={() => setActionError('')} />} />
        )}
        {vms === null ? (
          !error && (
            <Bullseye>
              <Spinner />
            </Bullseye>
          )
        ) : vms.length === 0 ? (
          <EmptyState titleText="No virtual machines" headingLevel="h2">
            <EmptyStateBody>This namespace has no virtual machines yet.</EmptyStateBody>
            {create}
          </EmptyState>
        ) : (
          <Table aria-label="Virtual machines" variant="compact">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Status</Th>
                <Th>Size</Th>
                <Th>Node</Th>
                <Th>IP addresses</Th>
                <Th>OS</Th>
                <Th screenReaderText="Actions" />
              </Tr>
            </Thead>
            <Tbody>
              {vms.map((vm) => (
                <Tr key={vm.name}>
                  <Td dataLabel="Name">
                    <Link to={`/ns/${encodeURIComponent(ns)}/vms/${encodeURIComponent(vm.name)}`}>{vm.name}</Link>
                  </Td>
                  <Td dataLabel="Status">
                    <VMStatusCell vm={vm} />
                  </Td>
                  <Td dataLabel="Size">{vmSize(vm)}</Td>
                  <Td dataLabel="Node">{vm.node || '—'}</Td>
                  <Td dataLabel="IP addresses">{vm.ips.join(', ') || '—'}</Td>
                  <Td dataLabel="OS">{vm.os || '—'}</Td>
                  <Td isActionCell>
                    <PowerMenu vm={vm} role={role} onError={setActionError} />
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </PageSection>
    </>
  )
}
