import { shallowMount } from '@vue/test-utils'
import { expect, it, vi } from 'vitest'
import NativeVideoPrices from '../NativeVideoPrices.vue'
import { nativeVideoCells } from '../videoTariffPricing'
import profiles from '../atlasVideoTariffProfiles.json'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
it('edits the exact whole-job amount without mutating provenance or auto-enabling free pricing', async () => {
  const model = 'google/veo3.1/text-to-video'
  const cell = nativeVideoCells(model)[0]
  const entry = { models: [model], billing_mode: 'video', intervals: [{ tier_label: cell.key, per_request_price: '9999999999.12345678' }],
    video_tariff: { version: 1, contract_revision: profiles[model].contractRevision, cells: {
      [cell.key]: { source: 'https://api.atlascloud.ai/api/v1/model/calculate', checked_at: '2026-10-22T00:00:00Z',
        upstream_price: '1.6', supplier_estimated: true, policy: 'fixed-customer-price-platform-variance-v1' },
    } } }
  const wrapper = shallowMount(NativeVideoPrices, { props: { entry: entry as never } })
  expect(wrapper.emitted('update')).toBeUndefined()
  const price = wrapper.find('input[data-video-price]')
  expect((price.element as HTMLInputElement).value).toBe('9999999999.12345678')
  await price.setValue('0')
  const edited = wrapper.emitted('update')!.at(-1)![0] as typeof entry
  expect(edited.intervals[0].per_request_price).toBe('0')
  expect(edited.video_tariff).toEqual(entry.video_tariff)
  expect(entry.intervals[0].per_request_price).toBe('9999999999.12345678')
})
