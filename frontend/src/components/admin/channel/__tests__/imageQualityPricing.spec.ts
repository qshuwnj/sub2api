import { describe, expect, it } from 'vitest'
import { isMiniQualityModelCard, isValidImageQualityPrice, setImageQualityPrice } from '../imageQualityPricing'
import { apiIntervalsToForm, formIntervalsToAPI, validateIntervals } from '../types'

describe('Mini native quality price editor', () => {
  it('recognizes exact endpoints, never names, wildcards or unrelated mixed models', () => {
    expect(isMiniQualityModelCard(['openai/gpt-image-1-mini/text-to-image'])).toBe(true)
    expect(isMiniQualityModelCard(['openai/gpt-image-1-mini/edit'])).toBe(true)
    expect(isMiniQualityModelCard(['openai/gpt-image-1-mini/*'])).toBe(false)
    expect(isMiniQualityModelCard(['openai/gpt-image-1-mini/edit', 'other'])).toBe(false)
  })
  it('keeps missing distinct from explicitly free, preserves native values and eight decimal money', () => {
    let rows = setImageQualityPrice([], 'low', '0')
    rows = setImageQualityPrice(rows, 'medium', '0.04')
    rows = setImageQualityPrice(rows, 'high', '0.12345678')
    expect(formIntervalsToAPI(rows).map(row => [row.tier_label, row.per_request_price])).toEqual([
      ['quality:low', 0], ['quality:medium', 0.04], ['quality:high', 0.12345678],
    ])
    rows = setImageQualityPrice(rows, 'medium', '')
    expect(rows.map(row => row.tier_label)).toEqual(['quality:low', 'quality:high'])
  })
  it.each(['-1', 'NaN', 'Infinity', 'bad', '0.123456789'])('rejects invalid tariffs instead of coercing them into free: %s', value => {
    expect(isValidImageQualityPrice(value)).toBe(false)
    expect(validateIntervals(setImageQualityPrice([], 'low', value), 'image', key => key)).toBe('admin.channels.form.imageQualityInvalid')
  })
  it('cannot silently leave quality pricing when the last price is cleared', () => {
    const rows = setImageQualityPrice(setImageQualityPrice([], 'low', '0.02'), 'low', '')
    expect(rows).toHaveLength(1)
    expect(rows[0].tier_label).toBe('quality:low')
    expect(validateIntervals(rows, 'image', key => key)).toBe('admin.channels.form.imageQualityInvalid')
  })
  it('can configure only low quality without saving the untouched enrollment draft as a price', () => {
    const draft = setImageQualityPrice([], 'medium', '')
    const rows = setImageQualityPrice(draft, 'low', '0.02')
    expect(validateIntervals(rows, 'image', key => key)).toBeNull()
    expect(rows.map(row => row.tier_label)).toEqual(['quality:low'])
  })
  it.each(['0.00000001', '0.0000001'])('can save, reload and edit the smallest supported tariffs: %s', price => {
    const reloaded = apiIntervalsToForm(formIntervalsToAPI(setImageQualityPrice([], 'low', price)))
    expect(validateIntervals(reloaded, 'image', key => key)).toBeNull()
    expect(isValidImageQualityPrice(reloaded[0].per_request_price)).toBe(true)
  })
  it.each([1e-9, 0.123456789, -1, Infinity, NaN, 1e12])('does not round invalid numeric tariffs into valid prices: %s', value => {
    expect(isValidImageQualityPrice(value)).toBe(false)
  })
})
