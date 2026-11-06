import { mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createElectricLogo } from '../electric-logo/renderer.js'
import HomeElectricLogo from '../HomeElectricLogo.vue'

vi.mock('../electric-logo/renderer.js', () => ({
  createElectricLogo: vi.fn(),
}))

describe('HomeElectricLogo static fallback', () => {
  let media: MediaQueryList
  let events: EventTarget

  beforeEach(() => {
    events = new EventTarget()
    media = {
      matches: false,
      addEventListener: events.addEventListener.bind(events),
      removeEventListener: events.removeEventListener.bind(events),
    } as unknown as MediaQueryList
    vi.spyOn(window, 'matchMedia').mockReturnValue(media)
    vi.stubGlobal('WebGL2RenderingContext', undefined)
  })

  afterEach(() => {
    vi.restoreAllMocks()
    vi.mocked(createElectricLogo).mockReset()
    vi.unstubAllGlobals()
  })

  it('preserves the configured brand and accessible name when WebGL is unavailable', async () => {
    const wrapper = mount(HomeElectricLogo, { props: { src: '/quota.svg', alt: 'QuotaFlow API' } })
    await wrapper.vm.$nextTick()
    expect(wrapper.get('[role="img"]').attributes('aria-label')).toBe('QuotaFlow API')
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('fallback')
    expect(wrapper.get('img').attributes('src')).toBe('/quota.svg')
    expect(wrapper.get('canvas').attributes('aria-hidden')).toBe('true')
    expect(wrapper.get('canvas').attributes('tabindex')).toBe('-1')
    wrapper.unmount()
  })

  it('does not create a graphics context when reduced motion is requested', () => {
    Object.assign(media, { matches: true })
    const context = vi.spyOn(HTMLCanvasElement.prototype, 'getContext')
    const wrapper = mount(HomeElectricLogo, { props: { src: '/quota.svg' } })
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('static')
    expect(context).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('responds to a changed motion preference without removing the brand image', async () => {
    const wrapper = mount(HomeElectricLogo, { props: { src: '/quota.svg' } })
    Object.assign(media, { matches: true })
    events.dispatchEvent(new Event('change'))
    await wrapper.vm.$nextTick()
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('static')
    expect(wrapper.get('img').attributes('src')).toBe('/quota.svg')
    Object.assign(media, { matches: false })
    events.dispatchEvent(new Event('change'))
    await wrapper.vm.$nextTick()
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('fallback')
    wrapper.unmount()
  })

  it('keeps the current brand on source changes and context loss', async () => {
    const wrapper = mount(HomeElectricLogo, { props: { src: '/old.svg' } })
    await wrapper.setProps({ src: '/current.svg', alt: 'Current brand' })
    await wrapper.get('canvas').trigger('webglcontextlost')
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('fallback')
    expect(wrapper.get('img').attributes('src')).toBe('/current.svg')
    expect(wrapper.get('[role="img"]').attributes('aria-label')).toBe('Current brand')
    wrapper.unmount()
  })

  it('uses the electric tone while a slow renderer initializes and after an error', async () => {
    vi.stubGlobal('WebGL2RenderingContext', class {})
    const dispose = vi.fn()
    vi.mocked(createElectricLogo).mockReturnValue({ dispose })
    const wrapper = mount(HomeElectricLogo, { props: { src: '/quota.svg' } })
    await wrapper.vm.$nextTick()

    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('loading')
    expect(wrapper.get('[role="img"]').attributes('data-tone')).toBe('dark')
    expect(wrapper.get('img').attributes('src')).toBe('/quota.svg')

    const options = vi.mocked(createElectricLogo).mock.calls[0]![2]
    options.onReady?.()
    await wrapper.vm.$nextTick()
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('animated')
    options.onError?.()
    await wrapper.vm.$nextTick()
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('fallback')
    expect(wrapper.get('[role="img"]').attributes('data-tone')).toBe('dark')
    wrapper.unmount()
    expect(dispose).toHaveBeenCalledOnce()
  })

  it('updates the fallback tone and ignores callbacks from a disposed renderer', async () => {
    vi.stubGlobal('WebGL2RenderingContext', class {})
    vi.mocked(createElectricLogo).mockImplementation(() => ({ dispose: vi.fn() }))
    const wrapper = mount(HomeElectricLogo)
    const oldOptions = vi.mocked(createElectricLogo).mock.calls[0]![2]
    await wrapper.setProps({ dark: false })

    oldOptions.onReady?.()
    await wrapper.vm.$nextTick()
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('loading')
    expect(wrapper.get('[role="img"]').attributes('data-tone')).toBe('light')

    const currentOptions = vi.mocked(createElectricLogo).mock.calls[1]![2]
    currentOptions.onReady?.()
    await wrapper.vm.$nextTick()
    expect(wrapper.get('[role="img"]').attributes('data-renderer')).toBe('animated')
    wrapper.unmount()
  })
})
