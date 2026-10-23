import { describe, expect, it } from 'vitest'
import { imageQualityProfile, verifiedImagePricePreset, imageQualitySizePrice, setImageQualitySizePrice } from '../imageQualityPricing'
import { formIntervalsToAPI, apiIntervalsToForm } from '../types'

describe('AtlasCloud native image tariff editor', () => {
  it('keeps ratio and resolution independent without inventing quality grades', () => {
    const profile = imageQualityProfile(['google/nano-banana-2/text-to-image'])
    expect(profile.qualities).toEqual(['none'])
    expect(profile.sizes).toContainEqual({
      value: 'controls:WyIiLCIyayIsIjE2OjkiXQ', ratio: '16:9', label: '2K', native: true,
    })
    expect(imageQualityProfile(['xai/grok-imagine-image-2.0/text-to-image']).qualities).toEqual(['low', 'medium'])
  })
  it('round trips exact native tariff keys instead of pretending that resolution is a pixel size', () => {
    const models = ['xai/grok-imagine-image-2.0/text-to-image']
    const rows = setImageQualitySizePrice([], 'low', 'controls:WyIiLCIyayIsIjE2OjkiXQ', '0.50', models)
    expect(rows[0].tier_label).toBe('quality:low:controls:WyIiLCIyayIsIjE2OjkiXQ')
    const reloaded = apiIntervalsToForm(formIntervalsToAPI(rows))
    expect(imageQualitySizePrice(reloaded, 'low', 'controls:WyIiLCIyayIsIjE2OjkiXQ')).toBe(0.5)
    expect(imageQualitySizePrice(reloaded, 'medium', 'controls:WyIiLCIyayIsIjE2OjkiXQ')).toBe('')
  })
  it('applies exchange and markup once to the verified discounted upstream price', () => {
    const preset = verifiedImagePricePreset(['alibaba/wan-2.5/image-edit'])
    expect(imageQualitySizePrice(preset, 'none', 'controls:WyIxMjgwKjEyODAiLCIiLCIiXQ')).toBe('0.17497872')
  })
})
