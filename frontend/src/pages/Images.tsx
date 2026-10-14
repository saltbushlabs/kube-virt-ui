import { useEffect, useState, type FormEvent } from 'react'
import { useParams } from 'react-router-dom'
import {
  Alert,
  AlertActionCloseButton,
  Button,
  EmptyState,
  EmptyStateBody,
  FileUpload,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  PageSection,
  Progress,
  TextInput,
  Title,
} from '@patternfly/react-core'
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table'
import { api, vmPath, type Image, type VMOptions } from '../api'
import { NamespaceTabs } from '../NamespaceTabs'
import { useSession } from '../session'
import { suggestImage, uploadImage, type UploadMeta, type UploadStage } from '../upload'
import { useApi } from '../useApi'

// CDI's phase names describe its internals ("UploadReady" means ready to
// *receive* data); translate them for users.
const phaseText: Record<string, string> = {
  Pending: 'Preparing',
  UploadScheduled: 'Preparing',
  UploadReady: 'Waiting for data',
  UploadInProgress: 'Processing',
  ImportScheduled: 'Processing',
  ImportInProgress: 'Processing',
}

function ImageStatus({ img }: { img: Image }) {
  if (img.problem) return <span style={{ color: 'var(--pf-t--global--text--color--status--danger--default)' }}>{img.problem}</span>
  if (img.phase === 'Succeeded') return <Label color="green">Ready</Label>
  return (
    <Label color="blue">
      {phaseText[img.phase] ?? img.phase ?? 'Preparing'}
      {img.progress ? ` ${img.progress}` : ''}
    </Label>
  )
}

function UploadModal({ ns, onClose }: { ns: string; onClose: () => void }) {
  const options = useApi<VMOptions>(vmPath(ns) + '/vm-options')
  const [file, setFile] = useState<File | null>(null)
  const [meta, setMeta] = useState<UploadMeta>({ name: '', type: 'iso', size: '' })
  const [stage, setStage] = useState<UploadStage | null>(null)
  const [error, setError] = useState('')
  // Name of the image a previous attempt in this modal actually created
  // (recorded only once creation has succeeded), so a retry resumes it
  // instead of failing with "already exists" — while a name that was
  // already taken before this modal opened keeps erroring.
  const [attempted, setAttempted] = useState('')
  const busy = stage !== null

  // Leaving the page aborts the upload, so ask first.
  useEffect(() => {
    if (!busy) return
    const warn = (e: BeforeUnloadEvent) => e.preventDefault()
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [busy])

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!file) return
    setError('')
    const resume = attempted === meta.name
    // 'waiting' is only reached after the create call succeeded (or was
    // resumed), so only then is the name ours to resume on a retry.
    const onStage = (s: UploadStage) => {
      if (s.stage === 'waiting') setAttempted(meta.name)
      setStage(s)
    }
    try {
      await uploadImage(ns, file, { ...meta, storageClass: meta.storageClass || undefined }, onStage, resume)
      onClose()
    } catch (err) {
      setError((err as Error).message)
      setStage(null)
    }
  }

  const stageText =
    stage?.stage === 'creating' ? 'Creating image…' : stage?.stage === 'waiting' ? 'Waiting for the upload server…' : 'Uploading…'

  return (
    <Modal variant="medium" isOpen onClose={busy ? undefined : onClose}>
      <ModalHeader title="Upload image" description="An ISO to attach as a CD-ROM, or a disk image (qcow2, raw, vmdk…) to clone into new VMs." />
      <ModalBody>
        <Form id="upload-form" onSubmit={submit}>
          {error && <Alert variant="danger" isInline title={error} />}
          <FormGroup label="File" isRequired fieldId="file">
            <FileUpload
              id="file"
              filename={file?.name ?? ''}
              filenamePlaceholder="Choose an .iso, .qcow2, .img or .raw file"
              browseButtonText="Choose file"
              hideDefaultPreview
              isDisabled={busy}
              onFileInputChange={(_, f) => {
                setFile(f)
                setMeta((m) => ({ ...suggestImage(f), storageClass: m.storageClass }))
              }}
              onClearClick={() => setFile(null)}
            />
          </FormGroup>
          <FormGroup label="Name" isRequired fieldId="img-name">
            <TextInput id="img-name" value={meta.name} isDisabled={busy} onChange={(_, v) => setMeta({ ...meta, name: v.toLowerCase() })} isRequired />
          </FormGroup>
          <Flex>
            <FlexItem>
              <FormGroup label="Type" fieldId="img-type">
                <FormSelect id="img-type" value={meta.type} isDisabled={busy} onChange={(_, v) => setMeta({ ...meta, type: v as 'iso' | 'disk' })}>
                  <FormSelectOption value="iso" label="ISO (CD-ROM)" />
                  <FormSelectOption value="disk" label="Disk image" />
                </FormSelect>
              </FormGroup>
            </FlexItem>
            <FlexItem>
              <FormGroup label="Disk size" fieldId="img-size">
                <TextInput id="img-size" value={meta.size} isDisabled={busy} onChange={(_, v) => setMeta({ ...meta, size: v })} style={{ width: 120 }} />
              </FormGroup>
            </FlexItem>
            <FlexItem>
              <FormGroup label="Storage class" fieldId="img-sc">
                <FormSelect id="img-sc" value={meta.storageClass ?? ''} isDisabled={busy} onChange={(_, v) => setMeta({ ...meta, storageClass: v })}>
                  <FormSelectOption value="" label="Cluster default" />
                  {(options.data?.storageClasses ?? []).map((sc) => (
                    <FormSelectOption key={sc.name} value={sc.name} label={sc.name} />
                  ))}
                </FormSelect>
              </FormGroup>
            </FlexItem>
          </Flex>
          {stage && (
            <Progress
              title={stageText}
              value={stage.stage === 'uploading' ? stage.percent : undefined}
              measureLocation={stage.stage === 'uploading' ? 'outside' : 'none'}
              aria-label="Upload progress"
            />
          )}
        </Form>
      </ModalBody>
      <ModalFooter>
        <Button type="submit" form="upload-form" isLoading={busy} isDisabled={busy || !file || !meta.name}>
          Upload
        </Button>
        <Button variant="link" isDisabled={busy} onClick={onClose}>
          Cancel
        </Button>
      </ModalFooter>
    </Modal>
  )
}

export function ImagesPage() {
  const { ns = '' } = useParams()
  const { me } = useSession()
  const isOwner = me?.memberships.find((m) => m.namespace === ns)?.role === 'owner'
  const images = useApi<Image[]>(vmPath(ns) + '/images')
  const [uploading, setUploading] = useState(false)
  const [error, setError] = useState('')
  const { reload } = images

  // Poll while CDI is still processing something.
  const pending = images.data?.some((i) => i.phase !== 'Succeeded' && !i.problem)
  useEffect(() => {
    const t = setInterval(reload, pending ? 2000 : 10000)
    return () => clearInterval(t)
  }, [reload, pending])

  async function remove(name: string) {
    if (!confirm(`Delete image ${name}? VMs created from it keep their own copy; VMs using it as a CD-ROM keep it until they are deleted.`)) return
    try {
      await api('DELETE', `${vmPath(ns)}/images/${encodeURIComponent(name)}`)
    } catch (e) {
      setError((e as Error).message)
    }
    reload()
  }

  const upload = isOwner && <Button onClick={() => setUploading(true)}>Upload image</Button>

  return (
    <>
      <PageSection>
        <Flex justifyContent={{ default: 'justifyContentSpaceBetween' }} alignItems={{ default: 'alignItemsCenter' }}>
          <FlexItem>
            <Title headingLevel="h1">{ns}</Title>
          </FlexItem>
          <FlexItem>{upload}</FlexItem>
        </Flex>
        <NamespaceTabs ns={ns} />
      </PageSection>
      <PageSection>
        {(error || images.error) && (
          <Alert
            variant="danger"
            isInline
            title={error || images.error}
            actionClose={
              <AlertActionCloseButton
                onClose={() => {
                  setError('')
                  images.clearError()
                }}
              />
            }
          />
        )}
        {images.data?.length === 0 ? (
          <EmptyState titleText="No images" headingLevel="h2">
            <EmptyStateBody>Upload an ISO to install an operating system, or a disk image to clone into new VMs.</EmptyStateBody>
            {upload}
          </EmptyState>
        ) : (
          <Table aria-label="Images" variant="compact">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Type</Th>
                <Th>Size</Th>
                <Th>Status</Th>
                <Th>Uploaded</Th>
                <Th screenReaderText="Actions" />
              </Tr>
            </Thead>
            <Tbody>
              {(images.data ?? []).map((img) => (
                <Tr key={img.name}>
                  <Td dataLabel="Name">{img.name}</Td>
                  <Td dataLabel="Type">{img.type === 'iso' ? 'ISO' : 'Disk image'}</Td>
                  <Td dataLabel="Size">{img.size}</Td>
                  <Td dataLabel="Status">
                    <ImageStatus img={img} />
                  </Td>
                  <Td dataLabel="Uploaded">{new Date(img.created).toLocaleString()}</Td>
                  <Td isActionCell>
                    {isOwner && (
                      <Button variant="link" isDanger onClick={() => remove(img.name)}>
                        Delete
                      </Button>
                    )}
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </PageSection>
      {uploading && (
        <UploadModal
          ns={ns}
          onClose={() => {
            setUploading(false)
            reload()
          }}
        />
      )}
    </>
  )
}
