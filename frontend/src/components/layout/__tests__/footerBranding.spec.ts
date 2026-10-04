import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

const dir = dirname(fileURLToPath(import.meta.url))
const homeViewSource = readFileSync(resolve(dir, '../../../views/HomeView.vue'), 'utf8')
const keyUsageViewSource = readFileSync(resolve(dir, '../../../views/KeyUsageView.vue'), 'utf8')

describe('public footer branding', () => {
  it.each([
    ['HomeView', homeViewSource],
    ['KeyUsageView', keyUsageViewSource],
  ])('%s keeps the copyright notice without the upstream GitHub link', (_name, source) => {
    expect(source).toContain("{{ t('home.footer.allRightsReserved') }}")
    expect(source).not.toContain(':href="githubUrl"')
    expect(source).not.toContain("const githubUrl = 'https://github.com/Wei-Shaw/sub2api'")
  })
})
