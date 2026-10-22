import { describe, expect, it } from 'vitest'
import { parseVideoPricingJSON, stringifyVideoPricingJSON, validateNativeVideoCard, importNativeVideoDraft, nativeVideoCells } from '../videoTariffPricing'
import profiles from '../atlasVideoTariffProfiles.json'
import { createHash } from 'node:crypto'
import { apiIntervalsToForm, formIntervalsToAPI } from '../types'

const model = 'google/veo3.1/text-to-video'
const controls = { aspect_ratio: '16:9', duration: 8, generate_audio: false, resolution: '720p' }
const key = 'video:v1:' + btoa(JSON.stringify([model, 't2v', controls])).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '')
const raw = `{"model_pricing":[{"platform":"openai","models":["${model}"],"billing_mode":"video","intervals":[{"tier_label":"${key}","per_request_price":9999999999.12345678}],"video_tariff":{"version":1,"contract_revision":"${'a'.repeat(64)}","cells":{"${key}":{"source":"https://api.atlascloud.ai/api/v1/model/calculate","checked_at":"2026-10-22T00:00:00Z","upstream_price":"1.6","supplier_estimated":true,"policy":"fixed-customer-price-platform-variance-v1"}}}}]}`
describe('native video administrator money boundary', () => {
  it('matches every Studio native key and typed control across all 87 endpoint profiles', () => {
    let count = 0
    for (const [endpoint, profile] of Object.entries(profiles)) {
      const cells = nativeVideoCells(endpoint)
      count += cells.length
      expect(createHash('sha256').update(cells.map(cell => cell.key).sort().join('\n')).digest('hex')).toBe(profile.keysSHA256)
    }
    expect(count).toBe(10751)
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
})
