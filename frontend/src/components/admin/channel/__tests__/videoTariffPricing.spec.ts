import { describe, expect, it } from 'vitest'
import { parseVideoPricingJSON, stringifyVideoPricingJSON, validateNativeVideoCard, importNativeVideoDraft, nativeVideoCells, newNativeVideoAdditions } from '../videoTariffPricing'
import profiles from '../atlasVideoTariffProfiles.json'
import r2vDraft from '../atlasSeedanceR2VTariffDraft.json'
import { createHash } from 'node:crypto'
import { apiIntervalsToForm, formIntervalsToAPI } from '../types'

const model = 'google/veo3.1/text-to-video'
const controls = { aspect_ratio: '16:9', duration: 8, generate_audio: false, resolution: '720p' }
const key = 'video:v1:' + btoa(JSON.stringify([model, 't2v', controls])).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '')
const raw = `{"model_pricing":[{"platform":"openai","models":["${model}"],"billing_mode":"video","intervals":[{"tier_label":"${key}","per_request_price":9999999999.12345678}],"video_tariff":{"version":1,"contract_revision":"${'a'.repeat(64)}","cells":{"${key}":{"source":"https://api.atlascloud.ai/api/v1/model/calculate","checked_at":"2026-10-22T00:00:00Z","upstream_price":"1.6","supplier_estimated":true,"policy":"fixed-customer-price-platform-variance-v1"}}}}]}`
describe('native video administrator money boundary', () => {
  it('matches every Studio native key and typed control across all 91 endpoint profiles', () => {
    let count = 0
    for (const [endpoint, profile] of Object.entries(profiles)) {
      const cells = nativeVideoCells(endpoint)
      count += cells.length
      expect(createHash('sha256').update(cells.map(cell => cell.key).sort().join('\n')).digest('hex')).toBe(profile.keysSHA256)
    }
    expect(count).toBe(13757)
  })
  it('includes complete fixed grids for all four Seedance R2V endpoints', () => {
    const expected = {
      'bytedance/seedance-2.0/reference-to-video': 504,
      'bytedance/seedance-2.0-mini/reference-to-video': 360,
      'bytedance/seedance-2.0-fast/reference-to-video': 360,
      'bytedance/seedance-2.5/reference-to-video': 1782,
    }
    for (const [model, count] of Object.entries(expected)) {
      const profile = profiles[model]
      expect(profile?.mode).toBe('r2v')
      expect(nativeVideoCells(model)).toHaveLength(count)
      expect(nativeVideoCells(model).every(cell => Number(cell.controls.duration) > 0)).toBe(true)
      expect(nativeVideoCells(model).every(cell => cell.controls.ratio !== 'adaptive')).toBe(true)
    }
  })
  it('ships an importable fixed-price draft for each R2V endpoint', () => {
    expect(r2vDraft.model_pricing).toHaveLength(4)
    for (const card of r2vDraft.model_pricing) {
      expect(validateNativeVideoCard(card)).toBeNull()
      expect(card.video_tariff.cells).toHaveProperty(card.intervals[0].tier_label)
      expect(card.video_tariff.cells[card.intervals[0].tier_label].supplier_estimated).toBe(true)
    }
  })
  it('retains exact numeric eight-decimal wire amounts through editable form and back to numeric JSON', () => {
    const parsed = parseVideoPricingJSON(raw)
    const rows = apiIntervalsToForm(parsed.model_pricing[0].intervals)
    expect(rows[0].per_request_price).toBe('9999999999.12345678')
    parsed.model_pricing[0].intervals = formIntervalsToAPI(rows)
    expect(stringifyVideoPricingJSON(parsed)).toContain('"per_request_price":9999999999.12345678')
    rows[0].per_request_price = '2.50000001'
    parsed.model_pricing[0].intervals = formIntervalsToAPI(rows)
    expect(stringifyVideoPricingJSON(parsed)).toContain('"per_request_price":2.50000001')
    expect(parsed.model_pricing[0].video_tariff.cells[key].supplier_estimated).toBe(true)
  })
  it('rejects overprecision rather than accepting binary64-rounded numeric inputs', () => {
    const parsed = parseVideoPricingJSON(raw.replace('9999999999.12345678', '0.100000000000000001'))
    expect(() => stringifyVideoPricingJSON(parsed)).toThrow(/price|precision/i)
    expect(() => stringifyVideoPricingJSON(parseVideoPricingJSON(raw.replace('9999999999.12345678', '10000000000.00000000')))).toThrow(/price/i)
  })
  it('rejects malformed keys, missing provenance, duplicate cells, missing != free and wrong native control types', () => {
    const card = parseVideoPricingJSON(raw).model_pricing[0]
    for (const mutate of [
      (value: any) => { value.intervals[0].tier_label = 'video:v1:invalid' },
      (value: any) => { value.video_tariff.cells = {} },
      (value: any) => { value.intervals.push({ ...value.intervals[0] }) },
      (value: any) => { value.intervals[0].per_request_price = '' },
      (value: any) => { value.intervals[0].per_request_price = '0' },
      (value: any) => { value.intervals[0].per_request_price = 'NaN' },
    ]) {
      const value = structuredClone(card)
      mutate(value)
      expect(validateNativeVideoCard(value)).not.toBeNull()
    }
    expect(validateNativeVideoCard(card)).not.toBeNull() // partial grids and wrong revision cannot be marked ready.
  })
  it('import rejects partial/untrusted cards and never mutates the existing draft on failure', () => {
    const existing = [{ models: ['other'], billing_mode: 'image' }]
    expect(() => importNativeVideoDraft(raw, existing as never)).toThrow()
    expect(existing).toEqual([{ models: ['other'], billing_mode: 'image' }])
  })
  it('accepts a complete native grid with manual retail overrides and rejects mistyped controls or overlapping imports', () => {
    const card = parseVideoPricingJSON(raw).model_pricing[0]
    card.video_tariff.contract_revision = profiles[model].contractRevision
    const provenance = card.video_tariff.cells[key]
    card.intervals = nativeVideoCells(model).map(cell => ({ tier_label: cell.key, per_request_price: '2.50000001' }))
    card.video_tariff.cells = Object.fromEntries(card.intervals.map((row: any) => [row.tier_label, { ...provenance }]))
    expect(validateNativeVideoCard(card)).toBeNull()
    expect(importNativeVideoDraft(stringifyVideoPricingJSON([card]), [])).toHaveLength(1)
    expect(() => importNativeVideoDraft(stringifyVideoPricingJSON([card, card]), [])).toThrow(/overlap/i)
    const first = card.intervals[0]
    first.per_request_price = '0'
    expect(validateNativeVideoCard(card)).toMatch(/free/i)
    card.video_tariff.cells[first.tier_label].explicit_free = true
    expect(validateNativeVideoCard(card)).toBeNull()
    const wrong = { ...controls, duration: '8' }
    first.tier_label = 'video:v1:' + btoa(JSON.stringify([model, 't2v', wrong])).replace(/=+$/, '')
    expect(validateNativeVideoCard(card)).toMatch(/native.*cell/i)
  })
  it('refreshes an exact existing model from a full-group draft without replacing retail prices or unrelated cards', () => {
    const imported = parseVideoPricingJSON(raw).model_pricing[0]
    imported.video_tariff.contract_revision = profiles[model].contractRevision
    const evidence = imported.video_tariff.cells[key]
    imported.intervals = nativeVideoCells(model).map(cell => ({ tier_label: cell.key, per_request_price: '5.25000000' }))
    imported.video_tariff.cells = Object.fromEntries(imported.intervals.map((row: any) => [row.tier_label, { ...evidence }]))
    const existing = structuredClone(imported)
    existing.intervals = existing.intervals.map((row: any) => ({ ...row, per_request_price: '7.75000000' }))
    existing.video_tariff.cells = Object.fromEntries(existing.intervals.map((row: any) => [row.tier_label, {
      ...evidence, checked_at: '2026-10-20T00:00:00Z', upstream_price: '1.5',
    }]))
    const unrelated = { models: ['unrelated/image'], billing_mode: 'image' }
    const merged = importNativeVideoDraft(stringifyVideoPricingJSON({ model_pricing: [imported, unrelated] }), [existing, unrelated] as never)
    expect(merged).toHaveLength(2)
    expect(merged[0].intervals.every((row: any) => row.per_request_price === '7.75000000')).toBe(true)
    expect(merged[0].video_tariff.cells[key].upstream_price).toBe('1.6')
    expect(merged[1]).toEqual(unrelated)
    expect(existing.video_tariff.cells[key].upstream_price).toBe('1.5')

    const conflictingPattern = { models: ['google/veo3.1/*'], billing_mode: 'token' }
    expect(() => importNativeVideoDraft(stringifyVideoPricingJSON([imported]), [conflictingPattern] as never)).toThrow(/overlap/i)
    expect(existing.intervals[0].per_request_price).toBe('7.75000000')
  })
  it('adds only new native video cards without re-adding existing non-video pricing', () => {
    const imported = parseVideoPricingJSON(raw).model_pricing[0]
    imported.video_tariff.contract_revision = profiles[model].contractRevision
    const evidence = imported.video_tariff.cells[key]
    imported.intervals = nativeVideoCells(model).map(cell => ({ tier_label: cell.key, per_request_price: '5.25000000' }))
    imported.video_tariff.cells = Object.fromEntries(imported.intervals.map((row: any) => [row.tier_label, { ...evidence }]))

    const image = { models: ['gpt-image-1'], billing_mode: 'image', image_input_price: 0.01 }
    const merged = importNativeVideoDraft(
      stringifyVideoPricingJSON({ model_pricing: [imported, image] }),
      [image] as never,
    )

    expect(merged).toHaveLength(2)
    expect(merged.filter(card => card.models[0] === 'gpt-image-1')).toHaveLength(1)
    expect(newNativeVideoAdditions(merged, [image] as never)).toEqual([imported])
  })
})
