// Only same-origin paths are accepted as redirect targets. Browsers treat
// '\' like '/' and strip tabs/newlines, so '/\evil.example' or '/\t/evil'
// would otherwise become protocol-relative; reject those outright, then
// resolve against our origin and keep only the path.
export function safeNext(
  next: string | null,
  origin: string = typeof window !== 'undefined' ? window.location.origin : 'https://kvui.invalid',
): string {
  if (!next || next[0] !== '/' || next[1] === '/' || next[1] === '\\') return '/'
  for (const ch of next) {
    const c = ch.charCodeAt(0)
    if (c < 0x20 || c === 0x7f) return '/'
  }
  try {
    const url = new URL(next, origin)
    if (url.origin !== origin) return '/'
    return url.pathname + url.search + url.hash
  } catch {
    return '/'
  }
}
