import { useState, type FormEvent, type ReactNode } from 'react'
import { Navigate, useNavigate, useSearchParams } from 'react-router-dom'
import {
  ActionGroup,
  Alert,
  Bullseye,
  Button,
  Card,
  CardBody,
  CardTitle,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  TextInput,
} from '@patternfly/react-core'
import { api } from '../api'
import { safeNext } from '../safeNext'
import { useSession } from '../session'

export function AuthCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Bullseye style={{ minHeight: '100vh', padding: 16 }}>
      <Card style={{ width: '100%', maxWidth: 440 }}>
        <CardTitle component="h1">{title}</CardTitle>
        <CardBody>{children}</CardBody>
      </Card>
    </Bullseye>
  )
}

export function LoginPage() {
  const { me, refresh } = useSession()
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const next = safeNext(params.get('next'))
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  if (me) return <Navigate to={next} replace />

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await api('POST', '/login', { username, password, code })
      await refresh()
      navigate(next, { replace: true })
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <AuthCard title="Log in to KubeVirt UI">
      <Form onSubmit={submit}>
        {error && <Alert variant="danger" isInline title={error} />}
        <FormGroup label="Username" isRequired fieldId="username">
          <TextInput id="username" autoComplete="username" value={username} onChange={(_, v) => setUsername(v)} isRequired />
        </FormGroup>
        <FormGroup label="Password" isRequired fieldId="password">
          <TextInput id="password" type="password" autoComplete="current-password" value={password} onChange={(_, v) => setPassword(v)} isRequired />
        </FormGroup>
        <FormGroup label="Authenticator code" isRequired fieldId="code">
          <TextInput id="code" autoComplete="one-time-code" inputMode="numeric" value={code} onChange={(_, v) => setCode(v)} isRequired />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>6-digit code from your app, or a recovery code.</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FormGroup>
        <ActionGroup>
          <Button type="submit" isBlock isLoading={busy} isDisabled={busy}>
            Log in
          </Button>
        </ActionGroup>
      </Form>
    </AuthCard>
  )
}
