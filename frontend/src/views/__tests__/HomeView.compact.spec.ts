import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, RouterLinkStub } from '@vue/test-utils'

import HomeView from '../HomeView.vue'

const { appStore, authStore } = vi.hoisted(() => ({
  appStore: {
    cachedPublicSettings: {} as Record<string, unknown>,
    siteName: 'Fallback site',
    siteLogo: '',
    docUrl: '',
    publicSettingsLoaded: true,
    fetchPublicSettings: vi.fn(),
  },
  authStore: {
    isAuthenticated: false,
    isAdmin: false,
    user: null as { email?: string } | null,
    checkAuth: vi.fn(),
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => appStore,
  useAuthStore: () => authStore,
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStore,
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

function mountHome(settings: Record<string, unknown> = {}) {
  appStore.cachedPublicSettings = {
    site_name: 'Test site',
    site_subtitle: 'Test subtitle',
    ...settings,
  }

  return mount(HomeView, {
    global: {
      stubs: {
        RouterLink: RouterLinkStub,
        LocaleSwitcher: { template: '<div data-testid="locale-switcher" />' },
        Icon: { template: '<span data-testid="icon" />' },
      },
    },
  })
}

function compactDestination(wrapper: ReturnType<typeof mountHome>) {
  return wrapper.get('[data-testid="compact-home"]').findComponent(RouterLinkStub).props('to')
}

function modelPlazaDestination(wrapper: ReturnType<typeof mountHome>) {
  return wrapper
    .findAllComponents(RouterLinkStub)
    .find((link) => link.props('to') === '/model-plaza')
    ?.props('to')
}

describe('HomeView compact mode', () => {
  afterEach(() => {
    window.history.replaceState({}, '', '/')
  })

  beforeEach(() => {
    authStore.isAuthenticated = false
    authStore.isAdmin = false
    authStore.user = null
    authStore.checkAuth.mockClear()
    appStore.fetchPublicSettings.mockClear()
    localStorage.clear()
    vi.spyOn(window, 'matchMedia').mockReturnValue({ matches: false } as MediaQueryList)
  })

  it('renders custom HTML ahead of compact mode', () => {
    const wrapper = mountHome({
      compact_home_enabled: true,
      home_content: '<section id="custom-home">Custom home</section>',
    })

    expect(wrapper.get('#custom-home').text()).toBe('Custom home')
    expect(wrapper.find('[data-testid="compact-home"]').exists()).toBe(false)
  })

  it('loads the style comparison only with the explicit local preview query', async () => {
    window.history.replaceState({}, '', '/home?preview=styles')
    const wrapper = mountHome({
      custom_menu_items: [{ id: 'workshop', label: '创意工坊', visibility: 'user' }],
    })
    await vi.dynamicImportSettled()
    await flushPromises()
    expect(wrapper.find('[data-testid="home-style-preview"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="default-home"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="home-style-preview"]').text()).toContain('Test site')
    wrapper.unmount()
  })

  it('switches the local polish without replacing the original content or background', async () => {
    localStorage.setItem('theme', 'light')
    window.history.replaceState({ marker: 'preserve' }, '', '/home?preview=home-polish')
    const wrapper = mountHome()
    await vi.dynamicImportSettled()
    await flushPromises()
    const canvas = wrapper.get('canvas').element
    const content = wrapper.get('main').text()
    expect(wrapper.get('[data-testid="default-home"]').classes()).toContain('home-polished')
    expect(wrapper.get('.terminal-container').exists()).toBe(true)
    expect(wrapper.findAll('.home-feature-card')).toHaveLength(3)
    expect(wrapper.get('button[data-finish="refined"]').attributes('aria-pressed')).toBe('true')

    await wrapper.get('button[data-finish="original"]').trigger('click')
    expect(wrapper.get('[data-testid="default-home"]').classes()).not.toContain('home-polished')
    expect(window.location.search).toBe('?preview=home-polish&finish=original')
    await wrapper.get('button[data-finish="refined"]').trigger('click')
    expect(wrapper.get('[data-testid="default-home"]').classes()).toContain('home-polished')
    expect(wrapper.get('main').text()).toBe(content)
    expect(wrapper.get('canvas').element).toBe(canvas)
    expect(window.history.state).toEqual({ marker: 'preserve' })
    expect(localStorage.getItem('theme')).toBe('light')
    wrapper.unmount()
  })

  it('restores the original comparison URL and follows history without recreating the canvas', async () => {
    window.history.replaceState({}, '', '/home?preview=home-polish&finish=original')
    const wrapper = mountHome()
    await vi.dynamicImportSettled()
    await flushPromises()
    const canvas = wrapper.get('canvas').element
    expect(wrapper.get('[data-testid="default-home"]').classes()).not.toContain('home-polished')
    expect(wrapper.get('button[data-finish="original"]').attributes('aria-pressed')).toBe('true')
    window.history.replaceState({}, '', '/home?preview=home-polish&finish=refined')
    window.dispatchEvent(new PopStateEvent('popstate'))
    await flushPromises()
    expect(wrapper.get('[data-testid="default-home"]').classes()).toContain('home-polished')
    expect(wrapper.get('canvas').element).toBe(canvas)
    wrapper.unmount()
  })

  it('replaces letter placeholders with provider marks only in the refined local preview', async () => {
    window.history.replaceState({}, '', '/home?preview=home-polish&finish=refined')
    const wrapper = mountHome()
    await vi.dynamicImportSettled()
    await flushPromises()
    const cards = wrapper.findAll('.home-provider-card')
    expect(cards.slice(0, 4).every(card => card.find('svg path').exists())).toBe(true)
    expect(cards.slice(0, 4).map(card => card.get('.home-provider-logo').text())).toEqual(['', '', '', ''])
    expect(cards.slice(0, 4).every(card => card.get('svg').attributes('aria-hidden') === 'true')).toBe(true)
    expect(cards[4].text()).toContain('home.providers.soon')

    await wrapper.get('button[data-finish="original"]').trigger('click')
    expect(wrapper.findAll('.home-provider-card > div').slice(0, 4).map(icon => icon.text())).toEqual(['C', 'G', 'G', 'A'])
    expect(wrapper.find('.home-provider-logo').exists()).toBe(false)
    wrapper.unmount()
  })

  it('uses the clean electric preview asset without replacing the configured original brand', async () => {
    window.history.replaceState({}, '', '/home?preview=home-polish&finish=refined')
    const wrapper = mountHome({ site_logo: '/quota-brand.svg' })
    await vi.dynamicImportSettled()
    await flushPromises()
    const background = wrapper.get('[data-testid="home-fibers"] canvas').element
    const logo = wrapper.get('header [data-testid="electric-logo"]')
    expect(logo.get('img').attributes('src')).toContain('/quotaflow-clean.png')
    expect(logo.attributes('role')).toBe('img')

    await wrapper.get('button[data-finish="original"]').trigger('click')
    expect(wrapper.find('[data-testid="electric-logo"]').exists()).toBe(false)
    expect(wrapper.get('header img').attributes('src')).toBe('/quota-brand.svg')
    expect(wrapper.get('[data-testid="home-fibers"] canvas').element).toBe(background)
    wrapper.unmount()
  })

  it.each(['/home', '/home?preview=ghost-fibers-v2&finish=refined'])(
    'ships the refined homepage without exposing comparison controls at %s',
    async (url) => {
      window.history.replaceState({}, '', url)
      const wrapper = mountHome()
      await vi.dynamicImportSettled()
      await flushPromises()
      expect(wrapper.get('[data-testid="default-home"]').classes()).toContain('home-polished')
      expect(wrapper.find('[data-testid="home-polish-controls"]').exists()).toBe(false)
      expect(wrapper.find('[data-testid="electric-logo"]').exists()).toBe(true)
      expect(wrapper.findAll('.home-provider-logo')).toHaveLength(4)
      wrapper.unmount()
    },
  )

  it('ignores a finish query outside the explicit local comparison', () => {
    window.history.replaceState({}, '', '/home?finish=original')
    const wrapper = mountHome()
    expect(wrapper.get('[data-testid="default-home"]').classes()).toContain('home-polished')
    expect(wrapper.find('[data-testid="home-polish-controls"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('does not apply polish over authoritative custom or compact home content', async () => {
    window.history.replaceState({}, '', '/home?preview=home-polish')
    const custom = mountHome({ home_content: '<section id="custom-polish">Custom</section>' })
    const compact = mountHome({ compact_home_enabled: true })
    await vi.dynamicImportSettled()
    await flushPromises()
    expect(custom.get('#custom-polish').text()).toBe('Custom')
    expect(compact.get('[data-testid="compact-home"]').exists()).toBe(true)
    expect(custom.find('[data-testid="home-polish-controls"]').exists()).toBe(false)
    expect(compact.find('[data-testid="home-polish-controls"]').exists()).toBe(false)
    custom.unmount()
    compact.unmount()
  })

  it('keeps custom and compact home settings authoritative even with a preview query', async () => {
    window.history.replaceState({}, '', '/home?preview=styles')
    const custom = mountHome({ home_content: '<section id="custom-preview">Custom</section>' })
    const compact = mountHome({ compact_home_enabled: true })
    await flushPromises()
    expect(custom.get('#custom-preview').text()).toBe('Custom')
    expect(custom.find('[data-testid="home-style-preview"]').exists()).toBe(false)
    expect(compact.get('[data-testid="compact-home"]').exists()).toBe(true)
    expect(compact.find('[data-testid="home-style-preview"]').exists()).toBe(false)
    custom.unmount()
    compact.unmount()
  })

  it('renders custom URL content ahead of compact mode', () => {
    const wrapper = mountHome({
      compact_home_enabled: true,
      home_content: ' https://example.com/home ',
    })

    expect(wrapper.get('iframe').attributes('src')).toBe('https://example.com/home')
    expect(wrapper.find('[data-testid="compact-home"]').exists()).toBe(false)
  })

  it('treats whitespace-only custom content as empty and selects compact mode', () => {
    const wrapper = mountHome({ compact_home_enabled: true, home_content: ' \n\t ' })

    expect(wrapper.get('[data-testid="compact-home"]').text()).toContain('Test site')
  })

  it.each([undefined, false])('selects the default home when compact mode is %s', (enabled) => {
    const settings = enabled === undefined ? {} : { compact_home_enabled: enabled }
    const wrapper = mountHome(settings)

    expect(wrapper.find('[data-testid="compact-home"]').exists()).toBe(false)
    expect(wrapper.find('.terminal-container').exists()).toBe(true)
  })

  it('places the decorative fiber background behind the default home without changing the login destination', () => {
    const wrapper = mountHome()

    expect(wrapper.get('[data-testid="home-fibers"]').attributes('aria-hidden')).toBe('true')
    expect(wrapper.findAllComponents(RouterLinkStub).every((link) => link.props('to') === '/login')).toBe(true)
    wrapper.unmount()
  })

  it('does not add the fiber background over custom or compact home content', () => {
    const compact = mountHome({ compact_home_enabled: true })
    const custom = mountHome({ home_content: '<section>Custom home</section>' })

    expect(compact.find('[data-testid="home-fibers"]').exists()).toBe(false)
    expect(custom.find('[data-testid="home-fibers"]').exists()).toBe(false)
    compact.unmount()
    custom.unmount()
  })

  it('opens the default homepage with the original dark fiber background', async () => {
    const wrapper = mountHome()

    expect(wrapper.get('[data-testid="default-home"]').classes()).toContain('dark')
    expect(wrapper.get('[data-testid="home-fibers"]').classes()).toContain('home-fibers--dark')
    await wrapper.get('button[title="home.switchToLight"]').trigger('click')
    expect(wrapper.get('[data-testid="default-home"]').classes()).not.toContain('dark')
    wrapper.unmount()
  })

  it('links unauthenticated visitors to login', () => {
    expect(compactDestination(mountHome({ compact_home_enabled: true }))).toBe('/login')
  })

  it('links authenticated users to their dashboard', () => {
    authStore.isAuthenticated = true

    expect(compactDestination(mountHome({ compact_home_enabled: true }))).toBe('/dashboard')
  })

  it('links administrators to the admin dashboard', () => {
    authStore.isAuthenticated = true
    authStore.isAdmin = true

    const wrapper = mountHome({ compact_home_enabled: true })
    expect(compactDestination(wrapper)).toBe('/admin/dashboard')
    expect(authStore.checkAuth).toHaveBeenCalledOnce()
    expect(appStore.fetchPublicSettings).not.toHaveBeenCalled()
  })

  it('shows the model plaza link to anonymous visitors when public access is enabled', () => {
    const wrapper = mountHome({
      compact_home_enabled: true,
      model_plaza_enabled: true,
      model_plaza_require_auth: false,
    })

    expect(modelPlazaDestination(wrapper)).toBe('/model-plaza')
  })

  it('hides the model plaza link from anonymous visitors when sign-in is required', () => {
    const wrapper = mountHome({
      compact_home_enabled: true,
      model_plaza_enabled: true,
      model_plaza_require_auth: true,
    })

    expect(modelPlazaDestination(wrapper)).toBeUndefined()
  })

  it('shows the model plaza link to authenticated visitors when sign-in is required', () => {
    authStore.isAuthenticated = true

    const wrapper = mountHome({
      compact_home_enabled: true,
      model_plaza_enabled: true,
      model_plaza_require_auth: true,
    })

    expect(modelPlazaDestination(wrapper)).toBe('/model-plaza')
  })

  it('shows the model plaza link in the default home header', () => {
    const wrapper = mountHome({
      model_plaza_enabled: true,
      model_plaza_require_auth: false,
    })

    expect(modelPlazaDestination(wrapper)).toBe('/model-plaza')
  })

  it('hides the model plaza link when the feature is disabled', () => {
    const wrapper = mountHome({
      compact_home_enabled: true,
      model_plaza_enabled: false,
      model_plaza_require_auth: false,
    })

    expect(modelPlazaDestination(wrapper)).toBeUndefined()
  })
})
