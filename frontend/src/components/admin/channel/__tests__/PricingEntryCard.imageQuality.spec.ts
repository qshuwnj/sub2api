import { shallowMount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import PricingEntryCard from '../PricingEntryCard.vue'
import ImageQualityPrices from '../ImageQualityPrices.vue'
import { validateIntervals, type PricingFormEntry } from '../types'

vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

const createEmptyPricingEntry = (): PricingFormEntry => ({
  models: [], billing_mode: 'image', input_price: null, output_price: null,
  cache_write_price: null, cache_read_price: null, image_input_price: null,
  image_output_price: null, per_request_price: null, intervals: [],
  time_pricing: { timezone: 'Asia/Shanghai', weekdays_only: false, periods: [] },
})

describe('group Mini quality pricing control', () => {
  it('displays reloaded tiny numeric tariffs as editable decimal text', () => {
    const wrapper = shallowMount(ImageQualityPrices, {
      props: { intervals: [{
        tier_label: 'quality:low', per_request_price: 1e-8,
      }] as never },
    })
    expect((wrapper.find('input').element as HTMLInputElement).value).toBe('0.00000001')
    expect(wrapper.find('[role=alert]').exists()).toBe(false)
  })
  it('requires a deliberate tariff before enabling, with no inherited default or size fallback', async () => {
    const entry = {
      ...createEmptyPricingEntry(), models: ['openai/gpt-image-1-mini/text-to-image'],
      billing_mode: 'image' as const, per_request_price: '0.9',
    }
    const wrapper = shallowMount(PricingEntryCard, { props: { entry, platform: 'openai', enableImageQualityPricing: true } })
    await wrapper.find('input[type=checkbox]').setValue(true)
    const next = wrapper.emitted('update')!.at(-1)![0] as typeof entry
    expect(next.per_request_price).toBeNull()
    expect(validateIntervals(next.intervals, 'image', key => key)).toBe('admin.channels.form.imageQualityInvalid')
    await wrapper.setProps({ entry: next })
    expect(wrapper.findComponent(ImageQualityPrices).exists()).toBe(true)
    expect(wrapper.text()).not.toContain('admin.channels.form.defaultImagePrice')
    expect(wrapper.find('[data-testid=reasoning-effort-multipliers]').exists()).toBe(false)
  })

  it('does not add the quality editor to another model or channel pricing', () => {
    for (const props of [
      { platform: 'openai', enableImageQualityPricing: false, models: ['openai/gpt-image-1-mini/edit'] },
      { platform: 'openai', enableImageQualityPricing: true, models: ['another-model'] },
    ]) {
      const wrapper = shallowMount(PricingEntryCard, {
        props: { ...props, entry: { ...createEmptyPricingEntry(), models: props.models, billing_mode: 'image' } },
      })
      expect(wrapper.find('input[type=checkbox]').exists()).toBe(false)
      expect(wrapper.findComponent(ImageQualityPrices).exists()).toBe(false)
    }
  })
})
