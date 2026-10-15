import { describe, expect, it } from 'vitest'

import {
  createVideoModelPricesForm,
  serializeVideoModelPrices,
  videoModelPriceFamilyRows,
  videoPriceResolutionsForModel
} from '../groupsVideoModelPricing'

describe('Grok video model pricing form', () => {
  it('provides editable rows for both canonical Grok video families', () => {
    const form = createVideoModelPricesForm()

    expect(videoModelPriceFamilyRows(form).map(({ key }) => key)).toEqual(expect.arrayContaining([
      'grok-imagine-video',
      'grok-imagine-video-1.5'
    ]))
    expect(form['grok-imagine-video']['480p']).toBeNull()
    expect(form['grok-imagine-video-1.5']['1080p']).toBeNull()
  })

  it('serializes only finite non-negative prices and preserves future families', () => {
    const form = createVideoModelPricesForm({
      'grok-imagine-video-2': { '1080p': 0.4 }
    })
    form['grok-imagine-video']['480p'] = 0.05
    form['grok-imagine-video']['720p'] = ''
    form['grok-imagine-video-1.5']['1080p'] = -1

    expect(serializeVideoModelPrices(form)).toEqual({
      'grok-imagine-video': { '480p': 0.05 },
      'grok-imagine-video-2': { '1080p': 0.4 }
    })
  })

  it('round-trips unknown model families so editing does not discard them', () => {
    const form = createVideoModelPricesForm({
      'grok-imagine-video-2': { '480p': 0.2 }
    })

    expect(videoModelPriceFamilyRows(form).map(({ key }) => key)).toContain(
      'grok-imagine-video-2'
    )
    expect(serializeVideoModelPrices(form)).toMatchObject({
      'grok-imagine-video-2': { '480p': 0.2 }
    })
  })

  it('provides exact AtlasCloud model rows and model-specific resolutions without guessed prices', () => {
    const form = createVideoModelPricesForm()
    const minimax = 'minimax/h3-developer/text-to-video'
    const gemini = 'google/gemini-omni-1.1-flash/text-to-video'
    const grok = 'xai/grok-imagine-video/text-to-video'
    expect(videoModelPriceFamilyRows(form).map(({ key }) => key)).toEqual(
      expect.arrayContaining([minimax, gemini, grok])
    )
    expect(videoPriceResolutionsForModel(minimax).map(({ key }) => key)).toEqual(['480p', '768p', '1440p-sr', '4k-sr'])
    expect(videoPriceResolutionsForModel(gemini).map(({ key }) => key)).toEqual(['360p', '720p', '1080p', '4k'])
    expect(form[minimax]['4k-sr']).toBeNull()
    expect(serializeVideoModelPrices(form)).toEqual({})
    form[minimax]['4k-sr'] = 0.4
    expect(serializeVideoModelPrices(form)).toEqual({ [minimax]: { '4k-sr': 0.4 } })
  })
})
