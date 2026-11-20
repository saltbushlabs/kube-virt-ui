import { api, ApiError, vmPath, type Image } from './api'

export interface UploadMeta {
  name: string
  type: 'iso' | 'disk'
  size: string
  storageClass?: string
}

export type UploadStage = { stage: 'creating' } | { stage: 'waiting' } | { stage: 'uploading'; percent: number }

const GiB = 1024 ** 3

// suggestImage derives a name, type and disk size from the chosen file.
// Disk sizes leave room for filesystem overhead; qcow2 images may expand,
// so they get at least 10 GiB.
export function suggestImage(file: File): UploadMeta {
  const base = file.name.replace(/\.[^.]+$/, '').toLowerCase()
  const name = base.replace(/[^a-z0-9-]+/g, '-').replace(/^-+/, '').slice(0, 63).replace(/-+$/, '') || 'image'
  const type = /\.iso$/i.test(file.name) ? 'iso' : 'disk'
  const gib = Math.ceil((file.size * 1.1) / GiB) + 1
  return { name, type, size: `${type === 'disk' ? Math.max(10, gib) : gib}Gi` }
}

function abortError(signal?: AbortSignal): unknown {
  return signal?.reason ?? new DOMException('Aborted', 'AbortError')
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(abortError(signal))
    const onAbort = () => {
      clearTimeout(timer)
      reject(abortError(signal))
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

// waitUntilReady reports whether the image is ready to receive data. It
// returns false when a previous attempt already uploaded it, so a retry
// after a *successful* send the browser misreported finishes instead of
// trying to upload into a completed image.
async function waitUntilReady(ns: string, name: string, signal?: AbortSignal): Promise<boolean> {
  const deadline = Date.now() + 5 * 60_000
  for (;;) {
    signal?.throwIfAborted()
    const img = (await api<Image[]>('GET', vmPath(ns) + '/images', undefined, signal)).find((i) => i.name === name)
    if (img?.problem) throw new Error(img.problem)
    if (img?.phase === 'UploadReady') return true
    if (img?.phase === 'Succeeded') return false
    if (Date.now() > deadline)
      throw new Error(`Timed out waiting for the image to be ready for upload (stuck in ${img?.phase || 'Pending'}). Delete it and try again.`)
    await sleep(2000, signal)
  }
}

// XMLHttpRequest rather than fetch: fetch cannot report upload progress.
function send(ns: string, name: string, file: File, onProgress: (percent: number) => void, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(abortError(signal))
    const xhr = new XMLHttpRequest()
    const onAbort = () => xhr.abort()
    const cleanup = () => signal?.removeEventListener('abort', onAbort)
    xhr.open('POST', `/api${vmPath(ns)}/images/${encodeURIComponent(name)}/upload`)
    xhr.setRequestHeader('X-Requested-With', 'kvui')
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')
    xhr.upload.onprogress = (e) => !signal?.aborted && e.lengthComputable && onProgress(Math.floor((e.loaded / e.total) * 100))
    xhr.onload = () => {
      cleanup()
      if (xhr.status === 204) return resolve()
      let msg = xhr.statusText
      try {
        msg = JSON.parse(xhr.responseText).error ?? msg
      } catch {
        /* not JSON, e.g. a proxy's error page */
      }
      reject(new ApiError(xhr.status, msg))
    }
    xhr.onerror = () => {
      cleanup()
      reject(new Error('Upload failed: network error'))
    }
    xhr.onabort = () => {
      cleanup()
      reject(abortError(signal))
    }
    signal?.addEventListener('abort', onAbort, { once: true })
    xhr.send(file)
  })
}

// uploadImage creates the image, waits for CDI's upload server and streams
// the file. CDI then converts the data; the image list shows that phase.
// Retrying after a failure (resume, set by the caller for a name it
// already attempted in this session) reuses the image the first attempt
// created — the create then conflicts — rather than failing on the
// conflict itself. Without resume a conflict is a real name clash.
export async function uploadImage(
  ns: string,
  file: File,
  meta: UploadMeta,
  onStage: (s: UploadStage) => void,
  resume = false,
  signal?: AbortSignal,
) {
  signal?.throwIfAborted()
  onStage({ stage: 'creating' })
  try {
    await api('POST', vmPath(ns) + '/images', meta, signal)
  } catch (e) {
    if (!resume || !(e instanceof ApiError) || e.status !== 409) throw e
  }
  signal?.throwIfAborted()
  onStage({ stage: 'waiting' })
  if (!(await waitUntilReady(ns, meta.name, signal))) return
  signal?.throwIfAborted()
  onStage({ stage: 'uploading', percent: 0 })
  await send(ns, meta.name, file, (percent) => onStage({ stage: 'uploading', percent }), signal)
}
