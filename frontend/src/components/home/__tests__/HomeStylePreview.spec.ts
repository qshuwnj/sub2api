import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, RouterLinkStub } from '@vue/test-utils'
import HomeStylePreview from '../HomeStylePreview.vue'
import { isLocalHomeStylePreview } from '../localHomePreview'

function mountPreview() {
  return mount(HomeStylePreview, {
    props: {
      siteName: 'QuotaFlow API',
      siteLogo: '',
      docUrl: 'https://example.com/docs',
      isAuthenticated: false,
      dashboardPath: '/dashboard',
      workshopPath: '/custom/workshop',
    },
    global: { stubs: { RouterLink: RouterLinkStub } },
  })
}

describe('local homepage style preview', () => {
  beforeEach(() => {
    window.history.replaceState({}, '', '/home?preview=styles')
    vi.spyOn(window, 'matchMedia').mockReturnValue({ matches: true } as MediaQueryList)
  })

  afterEach(() => {
    window.history.replaceState({}, '', '/')
    vi.restoreAllMocks()
  })

  it.each(['127.0.0.1', 'localhost', '[::1]'])('allows preview on development loopback %s', (hostname) => {
    expect(isLocalHomeStylePreview(true, hostname, '?preview=styles')).toBe(true)
  })

  it('never enables preview in production, on remote hosts, or without the explicit query', () => {
    expect(isLocalHomeStylePreview(false, 'localhost', '?preview=styles')).toBe(false)
    expect(isLocalHomeStylePreview(true, 'quotaflowai.com', '?preview=styles')).toBe(false)
    expect(isLocalHomeStylePreview(true, 'localhost', '?preview=ghost-fibers-v2')).toBe(false)
  })

  it('keeps polish gated to its own explicit local development preview', () => {
    expect(isLocalHomeStylePreview(true, 'localhost', '?preview=home-polish', 'home-polish')).toBe(true)
    expect(isLocalHomeStylePreview(false, 'localhost', '?preview=home-polish', 'home-polish')).toBe(false)
    expect(isLocalHomeStylePreview(true, 'quotaflowai.com', '?preview=home-polish', 'home-polish')).toBe(false)
    expect(isLocalHomeStylePreview(true, 'localhost', '?preview=styles', 'home-polish')).toBe(false)
  })

  it('switches three distinct layouts while preserving the same background canvas', async () => {
    const wrapper = mountPreview()
    const canvas = wrapper.get('canvas').element
    expect(wrapper.get('main').attributes('data-style')).toBe('a')

    for (const style of ['b', 'c', 'a']) {
      await wrapper.get(`button[data-style="${style}"]`).trigger('click')
      expect(wrapper.get('main').attributes('data-style')).toBe(style)
      expect(wrapper.get(`button[data-style="${style}"]`).attributes('aria-pressed')).toBe('true')
      expect(wrapper.get('canvas').element).toBe(canvas)
      expect(new URLSearchParams(window.location.search).get('style')).toBe(style)
    }
    expect(wrapper.findAll('[data-testid="home-fibers"]')).toHaveLength(1)
    wrapper.unmount()
  })

  it('restores a shared variant URL and defaults invalid variants to A without changing theme', () => {
    localStorage.setItem('theme', 'light')
    window.history.replaceState({}, '', '/home?preview=styles&style=c')
    const restored = mountPreview()
    expect(restored.get('main').attributes('data-style')).toBe('c')
    restored.unmount()

    window.history.replaceState({}, '', '/home?preview=styles&style=invalid')
    const fallback = mountPreview()
    expect(fallback.get('main').attributes('data-style')).toBe('a')
    expect(localStorage.getItem('theme')).toBe('light')
    fallback.unmount()
  })

  it('uses the actual configured workshop and authentication destinations', async () => {
    const wrapper = mountPreview()
    await wrapper.get('button[data-style="c"]').trigger('click')
    const destinations = () => wrapper.findAllComponents(RouterLinkStub).map((link) => link.props('to'))
    expect(destinations()).toContain('/login')
    expect(destinations()).toContain('/login?redirect=%2Fcustom%2Fworkshop')
    await wrapper.setProps({ isAuthenticated: true, dashboardPath: '/admin/dashboard' })
    expect(destinations()).toContain('/admin/dashboard')
    expect(destinations()).toContain('/custom/workshop')
    expect(wrapper.get('a[href="https://example.com/docs"]').attributes('rel')).toContain('noopener')
    wrapper.unmount()
  })
})
