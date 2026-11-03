import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import MagicRingsBackground from '../MagicRingsBackground.vue'
import { createMagicRings } from '../magic-rings/renderer.js'

vi.mock('../magic-rings/renderer.js', () => ({ createMagicRings: vi.fn() }))

describe('MagicRingsBackground', () => {
  const dispose = vi.fn()
  let reduced: boolean
  let listeners: Map<string, () => void>
  let removeListener: ReturnType<typeof vi.fn>

  beforeEach(() => {
    vi.clearAllMocks()
    reduced = false
    listeners = new Map()
    removeListener = vi.fn()
    vi.stubGlobal('WebGL2RenderingContext', class {})
    vi.stubGlobal('matchMedia', (query: string) => ({
      get matches() { return query.includes('reduced-motion') && reduced },
      addEventListener: (_: string, listener: () => void) => listeners.set(query, listener),
      removeEventListener: removeListener,
    }))
    vi.mocked(createMagicRings).mockReturnValue({ dispose })
  })

  afterEach(() => vi.unstubAllGlobals())

  it('runs the original renderer as a decorative, non-focusable background', () => {
    const wrapper = mount(MagicRingsBackground)
    expect(createMagicRings).toHaveBeenCalledWith(
      expect.any(HTMLElement), expect.any(HTMLCanvasElement),
      expect.objectContaining({ color: '#A855F7', colorTwo: '#6366F1', intro: false }),
    )
    expect(wrapper.attributes('aria-hidden')).toBe('true')
    expect(wrapper.find('canvas').attributes('tabindex')).toBe('-1')
    wrapper.unmount()
    expect(dispose).toHaveBeenCalledOnce()
    expect(removeListener).toHaveBeenCalledTimes(2)
  })

  it('does not initialize WebGL for reduced motion', async () => {
    reduced = true
    const wrapper = mount(MagicRingsBackground)
    await flushPromises()
    expect(createMagicRings).not.toHaveBeenCalled()
    expect(wrapper.attributes('data-renderer')).toBe('static')
    expect(wrapper.findAll('.login-magic-rings__static span')).toHaveLength(4)
    wrapper.unmount()
  })

  it('falls back safely without WebGL or if initialization fails', async () => {
    vi.stubGlobal('WebGL2RenderingContext', undefined)
    const wrapper = mount(MagicRingsBackground)
    await flushPromises()
    expect(createMagicRings).not.toHaveBeenCalled()
    expect(wrapper.attributes('data-renderer')).toBe('fallback')
    wrapper.unmount()

    vi.stubGlobal('WebGL2RenderingContext', class {})
    vi.mocked(createMagicRings).mockImplementation(() => { throw new Error('No GPU') })
    const failed = mount(MagicRingsBackground)
    await flushPromises()
    expect(failed.findAll('.login-magic-rings__static span')).toHaveLength(4)
    failed.unmount()
  })

  it('disposes on context loss and restarts after restoration', async () => {
    const wrapper = mount(MagicRingsBackground)
    await wrapper.find('canvas').trigger('webglcontextlost')
    expect(dispose).toHaveBeenCalledOnce()
    expect(wrapper.attributes('data-renderer')).toBe('fallback')
    await wrapper.find('canvas').trigger('webglcontextrestored')
    expect(createMagicRings).toHaveBeenCalledTimes(2)
    expect(wrapper.attributes('data-renderer')).toBe('animated')
    wrapper.unmount()
  })

  it('responds to changes in reduced-motion preference', async () => {
    const wrapper = mount(MagicRingsBackground)
    reduced = true
    listeners.get('(prefers-reduced-motion: reduce)')?.()
    await flushPromises()
    expect(dispose).toHaveBeenCalledOnce()
    expect(wrapper.attributes('data-renderer')).toBe('static')
    reduced = false
    listeners.get('(prefers-reduced-motion: reduce)')?.()
    await flushPromises()
    expect(createMagicRings).toHaveBeenCalledTimes(2)
    expect(wrapper.attributes('data-renderer')).toBe('animated')
    wrapper.unmount()
  })
})
