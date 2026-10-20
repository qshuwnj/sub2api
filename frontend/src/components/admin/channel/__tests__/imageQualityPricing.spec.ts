import { describe, expect, it } from 'vitest'
import { isMiniQualityModelCard, isImageQualityModelCard, imageQualityProfile, verifiedImagePricePreset, isValidImageQualityPrice, setImageQualityPrice, setImageQualitySizePrice, imageQualitySizePrice } from '../imageQualityPricing'
import { apiIntervalsToForm, formIntervalsToAPI, validateIntervals } from '../types'

describe('Mini native quality price editor', () => {
  it('uses each exact OpenAI endpoint native size and quality contract', () => {
    expect(isImageQualityModelCard(['openai/gpt-image-2/text-to-image'])).toBe(true)
    expect(isImageQualityModelCard(['openai/gpt-image-2/text-to-image', 'openai/gpt-image-1/edit'])).toBe(false)
    expect(isImageQualityModelCard(['openai/gpt-image-2/*'])).toBe(false)
    const developer = imageQualityProfile(['openai/gpt-image-2.5-flare-developer/edit'])
    expect(developer.qualities).toEqual(['none'])
    expect(developer.sizes.some(size => size.value === '3840x2160')).toBe(true)
    const edit = imageQualityProfile(['openai/gpt-image-2.5-flare/edit'])
    expect(edit.sizes.some(size => size.value === '1024x1024')).toBe(false)
    expect(edit.qualities).toContain('max')
    const both = imageQualityProfile(['openai/gpt-image-2.5-flare/text-to-image', 'openai/gpt-image-2.5-flare/edit'])
    expect(both.sizes.some(size => size.value === '1024x1280')).toBe(false)
  })
  it('uses endpoint-specific verified presets and never combines differing prices', () => {
    const model = 'openai/gpt-image-1.5'
    const text = verifiedImagePricePreset([`${model}/text-to-image`])
    const edit = verifiedImagePricePreset([`${model}/edit`])
    expect(text).toHaveLength(9)
    expect(edit).toHaveLength(9)
    expect(imageQualitySizePrice(text, 'low', '1536x1024')).toBe('0.08675760')
    expect(imageQualitySizePrice(edit, 'low', '1536x1024')).toBe('0.08675760')
    expect(verifiedImagePricePreset([`${model}/text-to-image`, `${model}/edit`])).toHaveLength(9)
    expect(verifiedImagePricePreset(['openai/gpt-image-2.5-flare/text-to-image', 'openai/gpt-image-2.5-flare/edit'])).toEqual([])
    const developer = verifiedImagePricePreset(['openai/gpt-image-2.5-flare-developer/edit'])
    expect(developer).toHaveLength(14)
    expect(developer.every(row => row.tier_label.startsWith('quality:none:size:') && !row.tier_label.endsWith('auto'))).toBe(true)
  })
  it('round-trips independently priced sizes without generic resolution tiers', () => {
    let rows = setImageQualitySizePrice([], 'low', '1024x1024', '0.0051')
    rows = setImageQualitySizePrice(rows, 'low', '1024x1536', '0.00612')
    rows = setImageQualitySizePrice(rows, 'high', '1536x1024', '0.05304')
    expect(formIntervalsToAPI(rows).map(row => [row.tier_label, row.per_request_price])).toEqual([
      ['quality:low:size:1024x1024', 0.0051],
      ['quality:low:size:1024x1536', 0.00612],
      ['quality:high:size:1536x1024', 0.05304],
    ])
    const reloaded = apiIntervalsToForm(formIntervalsToAPI(rows))
    expect(imageQualitySizePrice(reloaded, 'high', '1536x1024')).toBe(0.05304)
    expect(imageQualitySizePrice(reloaded, 'high', '1024x1024')).toBe('')
    expect(validateIntervals(reloaded, 'image', key => key)).toBeNull()
  })
  it('expands a legacy flat card without changing its other size prices on first edit', () => {
    const legacy = setImageQualityPrice(setImageQualityPrice([], 'low', '0'), 'medium', '0.02')
    expect(imageQualitySizePrice(legacy, 'medium', '1536x1024')).toBe('0.02')
    const rows = setImageQualitySizePrice(legacy, 'medium', '1024x1536', '0.0153')
    expect(rows).toHaveLength(6)
    expect(rows.every(row => row.tier_label.includes(':size:'))).toBe(true)
    expect(imageQualitySizePrice(rows, 'medium', '1024x1024')).toBe('0.02')
    expect(imageQualitySizePrice(rows, 'medium', '1024x1536')).toBe('0.0153')
    expect(imageQualitySizePrice(rows, 'low', '1536x1024')).toBe('0')
    expect(legacy.map(row => row.tier_label)).toEqual(['quality:low', 'quality:medium'])
  })
  it('retains enrollment when the last size price is cleared and never falls back across sizes', () => {
    const draft = setImageQualityPrice([], 'medium', '')
    let rows = setImageQualitySizePrice(draft, 'low', '1024x1024', '0.0051')
    expect(rows).toHaveLength(1)
    rows = setImageQualitySizePrice(rows, 'low', '1024x1024', '')
    expect(rows).toHaveLength(1)
    expect(validateIntervals(rows, 'image', key => key)).toBe('admin.channels.form.imageQualityInvalid')
    rows = setImageQualitySizePrice(rows, 'high', '1024x1536', '0.00000001')
    expect(rows).toHaveLength(1)
    expect(imageQualitySizePrice(rows, 'high', '1024x1024')).toBe('')
    expect(validateIntervals(rows, 'image', key => key)).toBeNull()
  })
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
