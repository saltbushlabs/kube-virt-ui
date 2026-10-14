import { useEffect, useState, type FormEvent } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import QRCode from 'qrcode'
import {
  ActionGroup,
  Alert,
  Bullseye,
  Button,
  ClipboardCopy,
  Content,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  List,
  ListItem,
  Spinner,
  TextInput,
} from '@patternfly/react-core'
import { api, type InviteInfo } from '../api'
import { useSession } from '../session'
import { AuthCard } from './Login'

type Step = 'account' | 'totp' | 'recovery'

function describe(info: InviteInfo) {
  const parts = []
  if (info.namespace) parts.push(`${info.role} of namespace ${info.namespace}`)
  if (info.makeAdmin) parts.push('an administrator')
  return parts.join(' and ')
}

export function InvitePage() {
  const { token = '' } = useParams()
  const { me, loading, refresh } = useSession()
  const navigate = useNavigate()
  const [info, setInfo] = useState<InviteInfo | null>(null)
  const [loadError, setLoadError] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const [step, setStep] = useState<Step>('account')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [totp, setTotp] = useState<{ secret: string; qr: string } | null>(null)
  const [code, setCode] = useState('')
  const [recovery, setRecovery] = useState<string[]>([])

  useEffect(() => {
    api<InviteInfo>('GET', `/invites/${token}`).then(setInfo, (e) => setLoadError(e.message))
  }, [token])

  async function run(fn: () => Promise<void>) {
    setBusy(true)
    setError('')
    try {
      await fn()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (loadError) {
    return (
      <AuthCard title="Invite unavailable">
        <Alert variant="danger" isInline title={loadError} />
      </AuthCard>
    )
  }
  if (!info || loading) {
    return (
      <Bullseye style={{ minHeight: '100vh' }}>
        <Spinner />
      </Bullseye>
    )
  }

  // Existing users accept the invite onto their account.
  if (me && step !== 'recovery') {
    return (
      <AuthCard title="Accept invite">
        {error && <Alert variant="danger" isInline title={error} />}
        <Content component="p">
          Signed in as <b>{me.user.username}</b>. This invite makes you {describe(info)}.
        </Content>
        <Button
          isBlock
          isLoading={busy}
          isDisabled={busy}
          onClick={() =>
            run(async () => {
              await api('POST', `/invites/${token}/accept`)
              await refresh()
              navigate('/')
            })
          }
        >
          Accept
        </Button>
      </AuthCard>
    )
  }

  if (step === 'account') {
    const submit = (e: FormEvent) => {
      e.preventDefault()
      // Mirrors the backend's limits so both are caught here instead of
      // at redeem time, after the QR scan: at least 12 characters, and at
      // most 256 UTF-8 *bytes* (the backend counts bytes, so measure with
      // TextEncoder, not String.length).
      if ([...password].length < 12) {
        setError('Password must be at least 12 characters')
        return
      }
      if (new TextEncoder().encode(password).length > 256) {
        setError('Password is too long (at most 256 bytes in UTF-8; accented and non-Latin characters count for more than one)')
        return
      }
      if (password !== confirm) {
        setError('Passwords do not match')
        return
      }
      run(async () => {
        const res = await api<{ secret: string; uri: string }>('POST', `/invites/${token}/totp`, { username })
        setTotp({ secret: res.secret, qr: await QRCode.toDataURL(res.uri) })
        setStep('totp')
      })
    }
    return (
      <AuthCard title="Create your account">
        <Form onSubmit={submit}>
          <Content component="p">You have been invited to become {describe(info)}.</Content>
          {error && <Alert variant="danger" isInline title={error} />}
          <FormGroup label="Username" isRequired fieldId="username">
            <TextInput id="username" autoComplete="username" value={username} onChange={(_, v) => setUsername(v)} isRequired />
          </FormGroup>
          <FormGroup label="Password" isRequired fieldId="password">
            <TextInput id="password" type="password" autoComplete="new-password" value={password} onChange={(_, v) => setPassword(v)} isRequired />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>At least 12 characters.</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Confirm password" isRequired fieldId="confirm">
            <TextInput id="confirm" type="password" autoComplete="new-password" value={confirm} onChange={(_, v) => setConfirm(v)} isRequired />
          </FormGroup>
          <ActionGroup>
            <Button type="submit" isBlock isLoading={busy} isDisabled={busy}>
              Continue
            </Button>
          </ActionGroup>
          <Content component="small">
            Already have an account? <Link to={`/login?next=/invite/${token}`}>Log in to accept</Link>
          </Content>
        </Form>
      </AuthCard>
    )
  }

  if (step === 'totp' && totp) {
    const submit = (e: FormEvent) => {
      e.preventDefault()
      run(async () => {
        const res = await api<{ recoveryCodes: string[] }>('POST', `/invites/${token}/redeem`, { username, password, code })
        setRecovery(res.recoveryCodes)
        setStep('recovery')
      })
    }
    return (
      <AuthCard title="Set up two-factor authentication">
        <Form onSubmit={submit}>
          {error && <Alert variant="danger" isInline title={error} />}
          <Content component="p">Scan this code with an authenticator app (Aegis, 1Password, Google Authenticator…).</Content>
          <Bullseye>
            <img src={totp.qr} alt="TOTP QR code" width={200} height={200} />
          </Bullseye>
          <FormGroup label="Or enter this key manually" fieldId="secret">
            <ClipboardCopy isReadOnly hoverTip="Copy" clickTip="Copied">
              {totp.secret}
            </ClipboardCopy>
          </FormGroup>
          <FormGroup label="Code from the app" isRequired fieldId="code">
            <TextInput id="code" autoComplete="one-time-code" inputMode="numeric" value={code} onChange={(_, v) => setCode(v)} isRequired />
          </FormGroup>
          <ActionGroup>
            <Button type="submit" isBlock isLoading={busy} isDisabled={busy}>
              Create account
            </Button>
            <Button variant="link" onClick={() => setStep('account')}>
              Back
            </Button>
          </ActionGroup>
        </Form>
      </AuthCard>
    )
  }

  return (
    <AuthCard title="Save your recovery codes">
      <Alert variant="warning" isInline title="These codes are shown only once">
        Each code can be used once instead of an authenticator code if you lose your device. Store them somewhere safe.
      </Alert>
      <List isPlain style={{ fontFamily: 'monospace', margin: '16px 0' }}>
        {recovery.map((c) => (
          <ListItem key={c}>{c}</ListItem>
        ))}
      </List>
      <ClipboardCopy isReadOnly hoverTip="Copy all" clickTip="Copied" variant="expansion">
        {recovery.join('\n')}
      </ClipboardCopy>
      <Button
        isBlock
        style={{ marginTop: 16 }}
        onClick={async () => {
          await refresh()
          navigate('/')
        }}
      >
        I have saved them
      </Button>
    </AuthCard>
  )
}
