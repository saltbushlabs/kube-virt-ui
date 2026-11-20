import { describe, expect, it } from 'vitest'
import { safeNext } from './safeNext'

const origin = 'https://vms.example.com'

describe('safeNext', () => {
  it('accepts same-origin relative paths and preserves query and hash', () => {
    expect(safeNext('/', origin)).toBe('/')
    expect(safeNext('/ns/demo', origin)).toBe('/ns/demo')
    expect(safeNext('/ns/demo/vms/web?tab=console#top', origin)).toBe('/ns/demo/vms/web?tab=console#top')
    expect(safeNext('/invite/abc123', origin)).toBe('/invite/abc123')
    expect(safeNext('/ns/a/../b', origin)).toBe('/ns/b')
  })

  it('rejects empty, null, and non-root-relative values', () => {
    expect(safeNext(null, origin)).toBe('/')
    expect(safeNext('', origin)).toBe('/')
    expect(safeNext('   ', origin)).toBe('/')
    expect(safeNext(' /ns/demo', origin)).toBe('/')
    expect(safeNext('ns/demo', origin)).toBe('/')
  })

  it('rejects absolute and scheme URLs', () => {
    expect(safeNext('https://evil.example/login', origin)).toBe('/')
    expect(safeNext('http://evil.example', origin)).toBe('/')
    expect(safeNext('https://vms.example.com/ns/demo', origin)).toBe('/')
    expect(safeNext('javascript:alert(1)', origin)).toBe('/')
    expect(safeNext('data:text/html,hi', origin)).toBe('/')
  })

  it('rejects protocol-relative and backslash redirect tricks', () => {
    expect(safeNext('//evil.example', origin)).toBe('/')
    expect(safeNext('//evil.example/path', origin)).toBe('/')
    expect(safeNext('///evil.example', origin)).toBe('/')
    expect(safeNext('/\\evil.example', origin)).toBe('/')
    expect(safeNext('\\evil.example', origin)).toBe('/')
  })

  it('rejects control characters including tabs, newlines, NUL, and DEL', () => {
    expect(safeNext('/\t/evil.example', origin)).toBe('/')
    expect(safeNext('/\n/evil.example', origin)).toBe('/')
    expect(safeNext('/\r/evil.example', origin)).toBe('/')
    expect(safeNext('/ns/\x00demo', origin)).toBe('/')
    expect(safeNext('/ns/\x1fdemo', origin)).toBe('/')
    expect(safeNext('/ns/\x7fdemo', origin)).toBe('/')
  })
})
