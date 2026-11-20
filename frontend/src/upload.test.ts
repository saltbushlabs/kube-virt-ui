import { describe, expect, it } from 'vitest'
import { suggestImage } from './upload'

const GiB = 1024 ** 3

describe('suggestImage', () => {
  it('detects ISO files case-insensitively and computes size with overhead', () => {
    expect(suggestImage({ name: 'Fedora-Workstation-41.iso', size: 2 * GiB })).toEqual({
      name: 'fedora-workstation-41',
      type: 'iso',
      size: '4Gi',
    })
    expect(suggestImage({ name: 'Debian.ISO', size: 0.5 * GiB })).toEqual({
      name: 'debian',
      type: 'iso',
      size: '2Gi',
    })
  })

  it('treats non-ISO files as disk images with at least 10Gi', () => {
    expect(suggestImage({ name: 'ubuntu-24.04.qcow2', size: 1 * GiB })).toEqual({
      name: 'ubuntu-24-04',
      type: 'disk',
      size: '10Gi',
    })
    expect(suggestImage({ name: 'large-disk.raw', size: 20 * GiB })).toEqual({
      name: 'large-disk',
      type: 'disk',
      size: '23Gi',
    })
  })

  it('normalizes special characters and falls back to "image" when empty', () => {
    expect(suggestImage({ name: 'Ubuntu 24.04_LTS (amd64).qcow2', size: 0 })).toEqual({
      name: 'ubuntu-24-04-lts-amd64',
      type: 'disk',
      size: '10Gi',
    })
    expect(suggestImage({ name: '---my_disk---.img', size: 0 })).toEqual({
      name: 'my-disk',
      type: 'disk',
      size: '10Gi',
    })
    expect(suggestImage({ name: '!!!.iso', size: 0 })).toEqual({
      name: 'image',
      type: 'iso',
      size: '1Gi',
    })
    expect(suggestImage({ name: '.iso', size: 0 })).toEqual({
      name: 'image',
      type: 'iso',
      size: '1Gi',
    })
  })

  it('truncates long names to the 63-character Kubernetes label limit', () => {
    const res = suggestImage({ name: `${'a'.repeat(80)}.qcow2`, size: 1 * GiB })
    expect(res.name).toBe('a'.repeat(63))
    expect(res.name.length).toBe(63)
  })
})
