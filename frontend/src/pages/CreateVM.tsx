import { useEffect, useState, type FormEvent } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import {
  ActionGroup,
  Alert,
  Breadcrumb,
  BreadcrumbItem,
  Button,
  Card,
  CardBody,
  CardTitle,
  Checkbox,
  ExpandableSection,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  FormSelectOptionGroup,
  HelperText,
  HelperTextItem,
  NumberInput,
  PageSection,
  Radio,
  TextArea,
  TextInput,
  Title,
} from '@patternfly/react-core'
import { api, vmPath, type CreateVMRequest, type DiskSource, type Image, type VMOptions } from '../api'
import { useApi } from '../useApi'

const clusterKinds = ['VirtualMachineClusterInstancetype', 'VirtualMachineClusterPreference']
const refKey = (r: { name: string; kind: string }) => `${r.kind}/${r.name}`

const sources: { value: DiskSource; label: string; urlLabel?: string; placeholder?: string; help: string }[] = [
  {
    value: 'registry',
    label: 'Container image (imported to a disk)',
    urlLabel: 'Image',
    placeholder: 'quay.io/containerdisks/fedora:latest',
    help: 'A containerDisk image from a registry, copied into a persistent disk.',
  },
  {
    value: 'http',
    label: 'Download from URL',
    urlLabel: 'Image URL',
    placeholder: 'https://download.fedoraproject.org/…/Fedora-Cloud-Base.qcow2',
    help: 'A qcow2, raw or ISO image on a public web server, copied into a persistent disk.',
  },
  {
    value: 'containerdisk',
    label: 'Container image (ephemeral)',
    urlLabel: 'Image',
    placeholder: 'quay.io/containerdisks/fedora:latest',
    help: 'Boots straight from the image. Changes to the disk are lost when the VM stops.',
  },
  { value: 'image', label: 'Uploaded disk image', help: 'A copy of a disk image uploaded on the Images tab.' },
  { value: 'blank', label: 'Blank disk', help: 'An empty disk. Attach an ISO below to install an operating system.' },
]

function help(text: string) {
  return (
    <FormHelperText>
      <HelperText>
        <HelperTextItem>{text}</HelperTextItem>
      </HelperText>
    </FormHelperText>
  )
}

export function CreateVMPage() {
  const { ns = '' } = useParams()
  const navigate = useNavigate()
  const options = useApi<VMOptions>(vmPath(ns) + '/vm-options')
  const images = useApi<Image[]>(vmPath(ns) + '/images')
  const ready = (type: Image['type']) => (images.data ?? []).filter((i) => i.type === type && i.phase === 'Succeeded')

  const [name, setName] = useState('')
  const [sizeMode, setSizeMode] = useState<'instancetype' | 'custom'>('instancetype')
  const [instancetype, setInstancetype] = useState('')
  const [cpus, setCpus] = useState(2)
  // Memory and disk size are kept as text so the field can be emptied
  // while typing instead of snapping back to 0.
  const [memory, setMemory] = useState('4')
  const [memoryUnit, setMemoryUnit] = useState<'Gi' | 'Mi'>('Gi')
  const [preference, setPreference] = useState('')
  const [diskSource, setDiskSource] = useState<DiskSource>('registry')
  const [diskUrl, setDiskUrl] = useState('')
  const [image, setImage] = useState('')
  const [cdrom, setCdrom] = useState('')
  const [diskSize, setDiskSize] = useState('20')
  const [storageClass, setStorageClass] = useState('')
  const [sshKeys, setSshKeys] = useState('')
  const [userData, setUserData] = useState('')
  const [start, setStart] = useState(true)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const opts = options.data
  // Fall back to a custom size where the cluster offers no instancetypes.
  useEffect(() => {
    if (opts && opts.instancetypes.length === 0) setSizeMode('custom')
  }, [opts])

  const source = sources.find((s) => s.value === diskSource)!
  const persistent = diskSource !== 'containerdisk'

  async function submit(e: FormEvent) {
    e.preventDefault()
    const req: CreateVMRequest = {
      name,
      diskSource,
      diskUrl: source.urlLabel ? diskUrl : undefined,
      image: diskSource === 'image' ? image : undefined,
      cdrom: cdrom || undefined,
      diskSize: persistent ? `${diskSize}Gi` : undefined,
      storageClass: persistent && storageClass ? storageClass : undefined,
      sshKeys: sshKeys.split('\n').map((k) => k.trim()).filter(Boolean),
      userData: userData.trim() ? userData : undefined,
      start,
    }
    if (sizeMode === 'instancetype') {
      const it = opts?.instancetypes.find((i) => refKey(i) === instancetype)
      if (!it) {
        setError('Choose an instance type')
        return
      }
      req.instancetype = { name: it.name, kind: it.kind }
    } else {
      req.cpus = cpus
      req.memory = `${memory}${memoryUnit}`
    }
    const pref = opts?.preferences.find((p) => refKey(p) === preference)
    if (pref) req.preference = { name: pref.name, kind: pref.kind }

    setBusy(true)
    setError('')
    try {
      await api('POST', vmPath(ns) + '/vms', req)
      navigate(`/ns/${encodeURIComponent(ns)}/vms/${encodeURIComponent(name)}`)
    } catch (err) {
      setError((err as Error).message)
      window.scrollTo({ top: 0, behavior: 'smooth' })
    } finally {
      setBusy(false)
    }
  }

  const grouped = <T extends { kind: string }>(items: T[], render: (i: T) => React.ReactNode) => {
    const cluster = items.filter((i) => clusterKinds.includes(i.kind))
    const local = items.filter((i) => !clusterKinds.includes(i.kind))
    return (
      <>
        {cluster.length > 0 && <FormSelectOptionGroup label="Cluster">{cluster.map(render)}</FormSelectOptionGroup>}
        {local.length > 0 && <FormSelectOptionGroup label={`Namespace ${ns}`}>{local.map(render)}</FormSelectOptionGroup>}
      </>
    )
  }

  return (
    <>
      <PageSection>
        <Breadcrumb>
          <BreadcrumbItem render={() => <Link to={`/ns/${encodeURIComponent(ns)}`}>{ns}</Link>} />
          <BreadcrumbItem isActive>Create virtual machine</BreadcrumbItem>
        </Breadcrumb>
        <Title headingLevel="h1" style={{ marginTop: 16 }}>
          Create virtual machine
        </Title>
      </PageSection>
      <PageSection>
        <Form onSubmit={submit} style={{ maxWidth: 760 }}>
          {(error || options.error) && <Alert variant="danger" isInline title={error || options.error} />}

          <FormGroup label="Name" isRequired fieldId="name">
            <TextInput id="name" value={name} onChange={(_, v) => setName(v.toLowerCase())} isRequired />
            {help('Lowercase letters, digits and hyphens.')}
          </FormGroup>

          <Card isCompact>
            <CardTitle>Size</CardTitle>
            <CardBody>
              <FormGroup role="radiogroup" fieldId="size-mode" isInline>
                <Radio
                  id="size-it"
                  name="size-mode"
                  label="Instance type"
                  isChecked={sizeMode === 'instancetype'}
                  isDisabled={!opts?.instancetypes.length}
                  onChange={() => setSizeMode('instancetype')}
                />
                <Radio id="size-custom" name="size-mode" label="Custom" isChecked={sizeMode === 'custom'} onChange={() => setSizeMode('custom')} />
              </FormGroup>
              {sizeMode === 'instancetype' ? (
                <FormGroup label="Instance type" isRequired fieldId="instancetype" style={{ marginTop: 16 }}>
                  <FormSelect id="instancetype" value={instancetype} onChange={(_, v) => setInstancetype(v)}>
                    <FormSelectOption value="" label="Choose…" isPlaceholder />
                    {grouped(opts?.instancetypes ?? [], (i) => (
                      <FormSelectOption key={refKey(i)} value={refKey(i)} label={`${i.name} · ${i.cpus} vCPU, ${i.memory}`} />
                    ))}
                  </FormSelect>
                  {help(opts?.instancetypes.find((i) => refKey(i) === instancetype)?.description ?? 'Predefined CPU and memory sizes.')}
                </FormGroup>
              ) : (
                <Flex style={{ marginTop: 16 }}>
                  <FlexItem>
                    <FormGroup label="vCPUs" fieldId="cpus">
                      <NumberInput
                        id="cpus"
                        value={cpus}
                        min={1}
                        max={128}
                        onMinus={() => setCpus((c) => Math.max(1, c - 1))}
                        onPlus={() => setCpus((c) => Math.min(128, c + 1))}
                        onChange={(e) => setCpus(Math.min(128, Math.max(1, Number((e.target as HTMLInputElement).value) || 1)))}
                      />
                    </FormGroup>
                  </FlexItem>
                  <FlexItem>
                    <FormGroup label="Memory" fieldId="memory">
                      <Flex spaceItems={{ default: 'spaceItemsSm' }} flexWrap={{ default: 'nowrap' }}>
                        <TextInput id="memory" type="number" min={1} value={memory} onChange={(_, v) => setMemory(v)} style={{ width: 110 }} />
                        <FormSelect aria-label="Memory unit" value={memoryUnit} onChange={(_, v) => setMemoryUnit(v as 'Gi' | 'Mi')} style={{ width: 90 }}>
                          <FormSelectOption value="Gi" label="GiB" />
                          <FormSelectOption value="Mi" label="MiB" />
                        </FormSelect>
                      </Flex>
                    </FormGroup>
                  </FlexItem>
                </Flex>
              )}
              {(opts?.preferences.length ?? 0) > 0 && (
                <FormGroup label="Operating system preference" fieldId="preference" style={{ marginTop: 16 }}>
                  <FormSelect id="preference" value={preference} onChange={(_, v) => setPreference(v)}>
                    <FormSelectOption value="" label="None" />
                    {grouped(opts!.preferences, (p) => (
                      <FormSelectOption key={refKey(p)} value={refKey(p)} label={p.displayName ? `${p.displayName} (${p.name})` : p.name} />
                    ))}
                  </FormSelect>
                  {help('Optional guest OS defaults such as disk bus and firmware.')}
                </FormGroup>
              )}
            </CardBody>
          </Card>

          <Card isCompact>
            <CardTitle>Boot disk</CardTitle>
            <CardBody>
              <FormGroup label="Source" fieldId="source">
                <FormSelect id="source" value={diskSource} onChange={(_, v) => setDiskSource(v as DiskSource)}>
                  {sources.map((s) => (
                    <FormSelectOption key={s.value} value={s.value} label={s.label} />
                  ))}
                </FormSelect>
                {help(source.help)}
              </FormGroup>
              {source.urlLabel && (
                <FormGroup label={source.urlLabel} isRequired fieldId="url" style={{ marginTop: 16 }}>
                  <TextInput id="url" value={diskUrl} placeholder={source.placeholder} onChange={(_, v) => setDiskUrl(v)} isRequired />
                </FormGroup>
              )}
              {diskSource === 'image' && (
                <FormGroup label="Disk image" isRequired fieldId="image" style={{ marginTop: 16 }}>
                  <FormSelect id="image" value={image} onChange={(_, v) => setImage(v)}>
                    <FormSelectOption value="" label={ready('disk').length ? 'Choose…' : 'No disk images uploaded yet'} isPlaceholder />
                    {ready('disk').map((i) => (
                      <FormSelectOption key={i.name} value={i.name} label={`${i.name} (${i.size})`} />
                    ))}
                  </FormSelect>
                  {help('The new disk must be at least as large as the image.')}
                </FormGroup>
              )}
              {persistent && (
                <Flex style={{ marginTop: 16 }}>
                  <FlexItem>
                    <FormGroup label="Disk size (GiB)" fieldId="disk-size">
                      <TextInput id="disk-size" type="number" min={1} value={diskSize} onChange={(_, v) => setDiskSize(v)} style={{ width: 140 }} />
                    </FormGroup>
                  </FlexItem>
                  <FlexItem>
                    <FormGroup label="Storage class" fieldId="sc">
                      <FormSelect id="sc" value={storageClass} onChange={(_, v) => setStorageClass(v)}>
                        <FormSelectOption value="" label="Cluster default" />
                        {(opts?.storageClasses ?? []).map((sc) => (
                          <FormSelectOption key={sc.name} value={sc.name} label={sc.default ? `${sc.name} (default)` : sc.name} />
                        ))}
                      </FormSelect>
                    </FormGroup>
                  </FlexItem>
                </Flex>
              )}
            </CardBody>
          </Card>

          <Card isCompact>
            <CardTitle>CD-ROM</CardTitle>
            <CardBody>
              <FormGroup label="ISO image" fieldId="cdrom">
                <FormSelect id="cdrom" value={cdrom} onChange={(_, v) => setCdrom(v)}>
                  <FormSelectOption value="" label="None" />
                  {ready('iso').map((i) => (
                    <FormSelectOption key={i.name} value={i.name} label={i.name} />
                  ))}
                </FormSelect>
                {help('Boots from the CD-ROM while the disk is empty, then from the disk once an OS is installed. Upload ISOs on the Images tab.')}
              </FormGroup>
            </CardBody>
          </Card>

          <Card isCompact>
            <CardTitle>Access</CardTitle>
            <CardBody>
              <FormGroup label="SSH public keys" fieldId="ssh">
                <TextArea id="ssh" value={sshKeys} onChange={(_, v) => setSshKeys(v)} rows={3} placeholder="ssh-ed25519 AAAA… you@laptop" />
                {help("One per line. Installed for the image's default user via cloud-init.")}
              </FormGroup>
              <ExpandableSection toggleText="Custom cloud-init user data" style={{ marginTop: 16 }}>
                <TextArea
                  aria-label="Cloud-init user data"
                  value={userData}
                  onChange={(_, v) => setUserData(v)}
                  rows={10}
                  style={{ fontFamily: 'monospace' }}
                  placeholder={'#cloud-config\npackages:\n  - nginx'}
                />
                {help('Replaces the generated cloud-config, including the SSH keys above. Visible to everyone in the namespace: do not put secrets here.')}
              </ExpandableSection>
            </CardBody>
          </Card>

          <Checkbox id="start" label="Start the virtual machine after creating it" isChecked={start} onChange={(_, v) => setStart(v)} />

          <ActionGroup>
            <Button type="submit" isLoading={busy} isDisabled={busy || !opts}>
              Create
            </Button>
            <Button variant="link" onClick={() => navigate(`/ns/${encodeURIComponent(ns)}`)}>
              Cancel
            </Button>
          </ActionGroup>
        </Form>
      </PageSection>
    </>
  )
}
