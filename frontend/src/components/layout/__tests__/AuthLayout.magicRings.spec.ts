import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AuthLayout from '../AuthLayout.vue'

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    siteName: 'QuotaFlow API',
    siteLogo: '/logo.svg',
    cachedPublicSettings: { site_subtitle: 'API Platform' },
    publicSettingsLoaded: true,
    fetchPublicSettings: vi.fn(),
  }),
}))

describe('AuthLayout Magic Rings design', () => {
  it('preserves the default layout for other authentication pages', () => {
    const wrapper = mount(AuthLayout, { slots: { default: '<form>Existing form</form>' } })
    expect(wrapper.classes()).not.toContain('login-rings')
    expect(wrapper.find('[data-testid="login-magic-rings"]').exists()).toBe(false)
    expect(wrapper.find('.auth-electric-logo').exists()).toBe(false)
    expect(wrapper.get('img').attributes('src')).toBe('/logo.svg')
    expect(wrapper.text()).toContain('Existing form')
    expect(wrapper.text()).toContain('QuotaFlow API')
    wrapper.unmount()
  })

  it('opts into the design without changing the saved application theme', async () => {
    const originalTheme = document.documentElement.className
    const wrapper = mount(AuthLayout, {
      props: { magicRings: true },
      slots: { default: '<form>Login</form>', footer: '<a>Register</a>' },
      global: {
        stubs: {
          MagicRingsBackground: { template: '<div data-testid="login-magic-rings" />' },
          HomeElectricLogo: {
            props: ['alt', 'dark'],
            template: '<div data-testid="electric-logo" role="img" :aria-label="alt" />',
          },
        },
      },
    })
    await flushPromises()
    expect(wrapper.classes()).toContain('login-rings')
    expect(wrapper.classes()).toContain('dark')
    expect(wrapper.find('[data-testid="login-magic-rings"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="electric-logo"]').attributes('aria-label')).toBe('QuotaFlow API')
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.text()).toContain('Register')
    expect(document.documentElement.className).toBe(originalTheme)
    wrapper.unmount()
  })
})
