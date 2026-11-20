import { useEffect, useRef, useState } from 'react'
import RFB from '@novnc/novnc'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { Alert, Button, Flex, FlexItem, Label } from '@patternfly/react-core'
import { vmPath } from './api'

type ConnState = 'connecting' | 'connected' | 'disconnected'

function consoleURL(ns: string, name: string, kind: 'vnc' | 'console'): string {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${scheme}://${location.host}/api${vmPath(ns, name)}/${kind}`
}

// Browsers hide websocket handshake status codes, so a refused connection
// can only be explained in general terms.
const refusedHelp = 'Could not connect. The VM must be running, and consoles need the operator or owner role.'

function Toolbar({ state, onReconnect, children }: { state: ConnState; onReconnect: () => void; children?: React.ReactNode }) {
  const color = state === 'connected' ? 'green' : state === 'connecting' ? 'blue' : 'grey'
  return (
    <Flex alignItems={{ default: 'alignItemsCenter' }} style={{ marginBottom: 8 }}>
      <FlexItem>
        <Label color={color}>{state}</Label>
      </FlexItem>
      {children}
      {state === 'disconnected' && (
        <FlexItem>
          <Button variant="secondary" onClick={onReconnect}>
            Reconnect
          </Button>
        </FlexItem>
      )}
    </Flex>
  )
}

export function VNCConsole({ ns, name }: { ns: string; name: string }) {
  return <VNCConsoleSession key={`${ns}\0${name}`} ns={ns} name={name} />
}

function VNCConsoleSession({ ns, name }: { ns: string; name: string }) {
  const screen = useRef<HTMLDivElement>(null)
  const rfb = useRef<RFB | null>(null)
  const [state, setState] = useState<ConnState>('connecting')
  const [error, setError] = useState('')
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let disposed = false
    let connected = false
    let closed = false
    const client = new RFB(screen.current!, consoleURL(ns, name, 'vnc'))
    client.scaleViewport = true
    client.background = '#000'
    const onConnect = () => {
      if (disposed) return
      connected = true
      setState('connected')
      setError('')
      client.focus()
    }
    const onDisconnect = () => {
      closed = true
      if (disposed) return
      setState('disconnected')
      if (!connected) setError(refusedHelp)
    }
    client.addEventListener('connect', onConnect)
    client.addEventListener('disconnect', onDisconnect)
    rfb.current = client
    return () => {
      disposed = true
      client.removeEventListener('connect', onConnect)
      client.removeEventListener('disconnect', onDisconnect)
      if (!closed) client.disconnect()
      rfb.current = null
    }
  }, [ns, name, attempt])

  return (
    <>
      <Toolbar
        state={state}
        onReconnect={() => {
          setState('connecting')
          setError('')
          setAttempt((a) => a + 1)
        }}
      >
        <FlexItem>
          <Button variant="secondary" isDisabled={state !== 'connected'} onClick={() => rfb.current?.sendCtrlAltDel()}>
            Send Ctrl+Alt+Del
          </Button>
        </FlexItem>
      </Toolbar>
      {error && <Alert variant="warning" isInline title={error} style={{ marginBottom: 8 }} />}
      <div ref={screen} style={{ height: '70vh', minHeight: 360, background: '#000' }} />
    </>
  )
}

export function SerialConsole({ ns, name }: { ns: string; name: string }) {
  return <SerialConsoleSession key={`${ns}\0${name}`} ns={ns} name={name} />
}

function SerialConsoleSession({ ns, name }: { ns: string; name: string }) {
  const container = useRef<HTMLDivElement>(null)
  const [state, setState] = useState<ConnState>('connecting')
  const [error, setError] = useState('')
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let disposed = false
    const term = new Terminal({ cursorBlink: true, fontSize: 14, scrollback: 5000 })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(container.current!)
    fit.fit()
    const resize = new ResizeObserver(() => fit.fit())
    resize.observe(container.current!)

    let opened = false
    const ws = new WebSocket(consoleURL(ns, name, 'console'))
    ws.binaryType = 'arraybuffer'
    ws.onopen = () => {
      if (disposed) return
      opened = true
      setState('connected')
      setError('')
      term.focus()
      // Many guests only print a prompt after input.
      term.writeln('\x1b[2m[connected, press Enter for a prompt]\x1b[0m')
    }
    ws.onmessage = (e) => {
      if (disposed) return
      term.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data))
    }
    ws.onclose = (e) => {
      if (disposed) return
      setState('disconnected')
      if (!opened) setError(refusedHelp)
      else term.writeln(`\r\n\x1b[2m[disconnected${e.reason ? ': ' + e.reason : ''}]\x1b[0m`)
    }
    const encoder = new TextEncoder()
    const input = term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
    })

    return () => {
      disposed = true
      input.dispose()
      ws.onopen = null
      ws.onmessage = null
      ws.onclose = null
      ws.close()
      resize.disconnect()
      term.dispose()
    }
  }, [ns, name, attempt])

  return (
    <>
      <Toolbar
        state={state}
        onReconnect={() => {
          setState('connecting')
          setError('')
          setAttempt((a) => a + 1)
        }}
      />
      {error && <Alert variant="warning" isInline title={error} style={{ marginBottom: 8 }} />}
      <div ref={container} style={{ height: '70vh', minHeight: 360, background: '#000', padding: 4 }} />
    </>
  )
}
