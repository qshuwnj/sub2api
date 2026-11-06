import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import GhostFibersBackground from '../GhostFibersBackground.vue'

describe('GhostFibersBackground fallback', () => {
  beforeEach(() => {
    vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
      matches: query.includes('reduced-motion'),
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList))
  })

  afterEach(() => vi.restoreAllMocks())

  it('uses a decorative static surface without starting WebGL when motion is reduced', () => {
    const context = vi.spyOn(HTMLCanvasElement.prototype, 'getContext')
    const wrapper = mount(GhostFibersBackground)
    expect(wrapper.attributes('data-renderer')).toBe('static')
    expect(wrapper.attributes('aria-hidden')).toBe('true')
    expect(context).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('keeps the homepage usable when WebGL is unavailable', async () => {
    vi.spyOn(window, 'matchMedia').mockReturnValue({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList)
    vi.stubGlobal('WebGL2RenderingContext', undefined)
    const wrapper = mount(GhostFibersBackground)
    await nextTick()
    expect(wrapper.attributes('data-renderer')).toBe('fallback')
    expect(wrapper.find('canvas').attributes('tabindex')).toBe('-1')
    wrapper.unmount()
    vi.unstubAllGlobals()
  })
})
