import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import {
  Alert,
  AlertActionCloseButton,
  Breadcrumb,
  BreadcrumbItem,
  Bullseye,
  Card,
  CardBody,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  PageSection,
  Spinner,
  Tab,
  Tabs,
  TabTitleText,
  Title,
} from '@patternfly/react-core'
import { useSession } from '../session'
import { diskNote, PowerMenu, useVMWatch, vmSize, VMStatus } from '../vms'
import { SerialConsole, VNCConsole } from '../consoles'

export function VMPage() {
  const { ns = '', name = '' } = useParams()
  const { me } = useSession()
  const navigate = useNavigate()
  const role = me?.memberships.find((m) => m.namespace === ns)?.role
  // The namespace stream is reused so the details update live.
  const { vms, error } = useVMWatch(ns)
  const vm = vms?.find((v) => v.name === name)
  const [actionError, setActionError] = useState('')
  const [tab, setTab] = useState<string | number>('details')
  // Consoles need a running instance and operator or owner rights.
  const consoleDisabled = !vm || !['Running', 'Paused'].includes(vm.status) || !role || role === 'viewer'
  const activeTab = consoleDisabled && tab !== 'details' ? 'details' : tab

  const field = (term: string, value: React.ReactNode) => (
    <DescriptionListGroup>
      <DescriptionListTerm>{term}</DescriptionListTerm>
      <DescriptionListDescription>{value || '—'}</DescriptionListDescription>
    </DescriptionListGroup>
  )

  return (
    <>
      <PageSection>
        <Breadcrumb>
          <BreadcrumbItem render={() => <Link to={`/ns/${encodeURIComponent(ns)}`}>{ns}</Link>} />
          <BreadcrumbItem isActive>{name}</BreadcrumbItem>
        </Breadcrumb>
        <Flex justifyContent={{ default: 'justifyContentSpaceBetween' }} alignItems={{ default: 'alignItemsCenter' }} style={{ marginTop: 16 }}>
          <FlexItem>
            <Title headingLevel="h1">
              {name} {vm && <VMStatus status={vm.status} />}
            </Title>
          </FlexItem>
          <FlexItem>{vm && <PowerMenu vm={vm} role={role} onError={setActionError} onDeleted={() => navigate(`/ns/${encodeURIComponent(ns)}`)} />}</FlexItem>
        </Flex>
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
        ) : !vm ? (
          <Alert variant="warning" isInline title={`Virtual machine ${name} not found in ${ns}`} />
        ) : (
          <>
          {vm.disks.filter((d) => d.problem).map((d) => (
            <Alert key={d.name} variant="danger" isInline title={`Disk ${d.name} cannot be provisioned`} style={{ marginBottom: 16 }}>
              {d.problem}
            </Alert>
          ))}
          <Tabs activeKey={activeTab} onSelect={(_, k) => setTab(k)} mountOnEnter unmountOnExit>
            <Tab eventKey="details" title={<TabTitleText>Details</TabTitleText>}>
              <Card style={{ marginTop: 16 }}>
                <CardBody>
                  <DescriptionList columnModifier={{ default: '1Col', md: '2Col' }}>
                    {field('Status', vm.status)}
                    {field('Run strategy', vm.runStrategy)}
                    {field('Size', vmSize(vm))}
                    {field('Operating system', vm.os)}
                    {field('Node', vm.node)}
                    {field('IP addresses', vm.ips.join(', '))}
                    {field('Created', new Date(vm.created).toLocaleString())}
                    {field('Your role', role)}
                    {vm.disks.length > 0 &&
                      field(
                        'Disks',
                        vm.disks.map((d) => (
                          <div key={d.name}>
                            {diskNote(d)?.text ?? `${d.name}: ready`}
                          </div>
                        )),
                      )}
                  </DescriptionList>
                </CardBody>
              </Card>
            </Tab>
            <Tab eventKey="vnc" title={<TabTitleText>Console</TabTitleText>} isDisabled={consoleDisabled}>
              <div style={{ marginTop: 16 }}>
                <VNCConsole ns={ns} name={name} />
              </div>
            </Tab>
            <Tab eventKey="serial" title={<TabTitleText>Serial console</TabTitleText>} isDisabled={consoleDisabled}>
              <div style={{ marginTop: 16 }}>
                <SerialConsole ns={ns} name={name} />
              </div>
            </Tab>
          </Tabs>
          </>
        )}
      </PageSection>
    </>
  )
}
