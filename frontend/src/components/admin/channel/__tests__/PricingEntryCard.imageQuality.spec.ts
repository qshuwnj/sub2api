import { shallowMount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import PricingEntryCard from '../PricingEntryCard.vue'
import ImageQualityPrices from '../ImageQualityPrices.vue'
import { validateIntervals, type PricingFormEntry } from '../types'
import { setImageQualityPrice } from '../imageQualityPricing'

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
  it('renders developer native prices without inventing a quality field', () => {
    const wrapper = shallowMount(ImageQualityPrices, {
      props: { models: ['openai/gpt-image-2.5-flare-developer/edit'], intervals: [] },
    })
    expect(wrapper.findAll('input')).toHaveLength(15) // Fourteen fixed sizes plus an unpriced automatic size.
    expect(wrapper.text()).toContain('admin.channels.form.imageQuality_none')
    expect(wrapper.text()).not.toContain('admin.channels.form.imageQuality_medium')
  })
  it('fills verified prices only on an explicit draft action', async () => {
    const wrapper = shallowMount(ImageQualityPrices, {
      props: { models: ['openai/gpt-image-2/text-to-image'], intervals: [] },
    })
    expect(wrapper.emitted('update')).toBeUndefined()
    await wrapper.find('[data-testid="image-price-preset"]').trigger('click')
    const rows = wrapper.emitted('update')![0][0] as PricingFormEntry['intervals']
    expect(rows).toHaveLength(42)
    expect(rows.find(row => row.tier_label === 'quality:low:size:1024x1024')?.per_request_price).toBe('0.01305600')
  })
  it('does not inherit another endpoint tariff when the model is replaced', async () => {
    const entry = {
      ...createEmptyPricingEntry(), models: ['openai/gpt-image-1-mini/text-to-image'],
      intervals: setImageQualityPrice([], 'medium', '0.01122'),
    }
    const wrapper = shallowMount(PricingEntryCard, { props: { entry, platform: 'openai', enableImageQualityPricing: true } })
    wrapper.findComponent({ name: 'ModelTagInput' }).vm.$emit('update:models', ['openai/gpt-image-2/text-to-image'])
    const next = wrapper.emitted('update')!.at(-1)![0] as PricingFormEntry
    expect(next.models).toEqual(['openai/gpt-image-2/text-to-image'])
    expect(next.intervals).toHaveLength(1)
    expect(next.intervals[0].tier_label).toBe('quality:medium:size:1024x1024')
    expect(next.intervals[0].per_request_price).toBe('')
    expect(validateIntervals(next.intervals, 'image', key => key)).toBe('admin.channels.form.imageQualityInvalid')
  })
  it('renders nine accessible size-quality inputs and edits the selected cell only', async () => {
    const wrapper = shallowMount(ImageQualityPrices, {
      props: { intervals: setImageQualityPrice([], 'medium', '0.01122') },
    })
    expect(wrapper.findAll('input')).toHaveLength(9)
    const portraitHigh = wrapper.find('input[aria-label="2:3 1024 × 1536 admin.channels.form.imageQuality_high USD"]')
    expect(portraitHigh.exists()).toBe(true)
    await portraitHigh.setValue('0.05304')
    const rows = wrapper.emitted('update')!.at(-1)![0] as PricingFormEntry['intervals']
    expect(rows.find(row => row.tier_label === 'quality:high:size:1024x1536')?.per_request_price).toBe('0.05304')
    expect(rows.find(row => row.tier_label === 'quality:medium:size:1024x1024')?.per_request_price).toBe('0.01122')
    expect(rows.filter(row => row.tier_label.startsWith('quality:medium:'))).toHaveLength(3)
  })
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
