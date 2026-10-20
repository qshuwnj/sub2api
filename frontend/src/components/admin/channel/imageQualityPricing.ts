import type { IntervalFormEntry } from './types'

const MINI_MODELS = new Set(['openai/gpt-image-1-mini/text-to-image', 'openai/gpt-image-1-mini/edit'])
export const IMAGE_QUALITIES = ['low', 'medium', 'high'] as const
export type ImageQuality = typeof IMAGE_QUALITIES[number]

export function isMiniQualityModelCard(models: string[]): boolean {
  return models.length > 0 && models.every(model => MINI_MODELS.has(model))
}

export function isValidImageQualityPrice(value: unknown): boolean {
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || value < 0 || value >= 1e12 || Number(value.toFixed(8)) !== value) return false
    value = imageQualityPriceText(value)
  }
  return typeof value === 'string' &&
    /^(?:0|[1-9]\d{0,11})(?:\.\d{1,8})?$/.test(value) && Number.isFinite(Number(value))
}

export function imageQualityPriceText(value: number | string | null | undefined): string {
  // Keep in-progress text unchanged; API numbers must not reopen as scientific notation.
  return typeof value === 'number' && Number.isFinite(value)
    ? value.toFixed(8).replace(/\.?0+$/, '')
    : value == null ? '' : String(value)
}

export function setImageQualityPrice(rows: IntervalFormEntry[], quality: ImageQuality, value: string): IntervalFormEntry[] {
  const result = rows.filter(row => row.tier_label !== `quality:${quality}` &&
    !(value !== '' && row.per_request_price === ''))
  // Retain an invalid draft when clearing the last tier; only the explicit
  // opt-out toggle may return a configured model to legacy pricing.
  if (value !== '' || result.length === 0) {
    result.push({
      min_tokens: 0, max_tokens: null, tier_label: `quality:${quality}`,
      input_price: null, output_price: null, cache_write_price: null, cache_write_1h_price: null,
      cache_read_price: null, input_multiplier: null, output_multiplier: null,
      cache_write_multiplier: null, cache_read_multiplier: null, per_request_price: value, sort_order: 0,
    })
  }
  return result.sort((a, b) => IMAGE_QUALITIES.indexOf(a.tier_label.slice(8) as ImageQuality) - IMAGE_QUALITIES.indexOf(b.tier_label.slice(8) as ImageQuality))
    .map((row, index) => ({ ...row, sort_order: index }))
}
