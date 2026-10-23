import type { IntervalFormEntry } from './types'
import nativeProfiles from './openaiImagePricingProfiles.json'
import verifiedCatalog from './openaiImagePriceCatalog.json'
import atlasProfiles from './atlasImagePricingProfiles.json'
import atlasCatalog from './atlasImagePriceCatalog.json'

const MINI_MODELS = new Set(['openai/gpt-image-1-mini/text-to-image', 'openai/gpt-image-1-mini/edit'])
export const IMAGE_QUALITIES = ['low', 'medium', 'high'] as const
export type ImageQuality = 'low' | 'medium' | 'high' | 'xhigh' | 'max' | 'auto' | 'none'
export const IMAGE_SIZES = [
  { value: '1024x1024', ratio: '1:1', label: '1024 × 1024' },
  { value: '1024x1536', ratio: '2:3', label: '1024 × 1536' },
  { value: '1536x1024', ratio: '3:2', label: '1536 × 1024' },
] as const
export type ImageSize = string
const profiles: Record<string, { sizes: string[]; qualities: string[] }> = nativeProfiles
const qualityOrder: ImageQuality[] = ['low', 'medium', 'high', 'xhigh', 'max', 'auto', 'none']
type NativeProfile = { sizes: { value: string; ratio: string; label: string }[]; qualities: string[] }
const native: Record<string, NativeProfile> = atlasProfiles
const canonical = (model: string) => model.replace(/\/(?:text-to-image|image-edit|edit)$/, '')
const tariffLabel = (quality: ImageQuality, size: string) =>
  `quality:${quality}:${size.startsWith('controls:') ? size : `size:${size}`}`

export function isImageQualityModelCard(models: string[]): boolean {
  return models.length > 0 && models.every(model => !!(profiles[model] || native[model]) && canonical(model) === canonical(models[0]))
}

export function imageQualityProfile(models: string[] = ['openai/gpt-image-1-mini/text-to-image']) {
  if (!isImageQualityModelCard(models)) return { sizes: [], qualities: [] as ImageQuality[] }
  if (native[models[0]]) return {
    sizes: native[models[0]].sizes
      .filter(size => models.every(model => native[model]?.sizes.some(other => other.value === size.value)))
      .map(({ value, ratio, label }) => ({ value, ratio, label, native: true })),
    qualities: qualityOrder.filter(quality => models.every(model => native[model]?.qualities.includes(quality))),
  }
  const first = profiles[models[0]]
  const gcd = (a: number, b: number): number => b ? gcd(b, a % b) : a
  return {
    sizes: first.sizes.filter(size => models.every(model => profiles[model].sizes.includes(size))).map(value => {
      const [width, height] = value.split('x').map(Number)
      const divisor = gcd(width, height)
      return { value, ratio: value === 'auto' ? 'Auto' : `${width / divisor}:${height / divisor}`, label: value.replace('x', ' × ') }
    }),
    qualities: qualityOrder.filter(quality => models.every(model => profiles[model].qualities.includes(quality))),
  }
}

export function verifiedImagePricePreset(models: string[]): IntervalFormEntry[] {
  if (!isImageQualityModelCard(models)) return []
  if (native[models[0]]) {
    const cells = atlasCatalog.cells.filter(cell => cell.model === models[0])
    if (models.some(model => {
      const other = atlasCatalog.cells.filter(cell => cell.model === model)
      return other.length !== cells.length || cells.some(cell => !other.some(candidate =>
        candidate.controlKey === cell.controlKey && candidate.quality === cell.quality && candidate.price === cell.price))
    })) return []
    return cells.map((cell, index) => ({
      ...setImageQualityPrice([], cell.quality as ImageQuality, cell.price)[0],
      tier_label: tariffLabel(cell.quality as ImageQuality, cell.controlKey), sort_order: index,
    }))
  }
  const cells = verifiedCatalog.cells.filter(cell => cell.model === models[0])
  // One card cannot express different text-to-image/edit prices.
  if (models.some(model => {
    const candidate = verifiedCatalog.cells.filter(cell => cell.model === model)
    return candidate.length !== cells.length || cells.some(cell =>
      !candidate.some(other => other.size === cell.size && other.quality === cell.quality && other.price === cell.price))
  })) return []
  return cells.map((cell, index) => ({
    ...setImageQualityPrice([], cell.quality as ImageQuality, cell.price)[0],
    tier_label: `quality:${cell.quality}:size:${cell.size}`, sort_order: index,
  }))
}

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
  return result.sort((a, b) => qualityOrder.indexOf(a.tier_label.slice(8) as ImageQuality) - qualityOrder.indexOf(b.tier_label.slice(8) as ImageQuality))
    .map((row, index) => ({ ...row, sort_order: index }))
}

export function imageQualitySizePrice(rows: IntervalFormEntry[], quality: ImageQuality, size: ImageSize): number | string {
  const sized = rows.some(row => /:(?:size|controls):/.test(row.tier_label))
  const label = sized ? tariffLabel(quality, size) : `quality:${quality}`
  return rows.find(row => row.tier_label === label)?.per_request_price ?? ''
}

export function setImageQualitySizePrice(rows: IntervalFormEntry[], quality: ImageQuality, size: ImageSize, value: string, models?: string[]): IntervalFormEntry[] {
  const profile = imageQualityProfile(models)
  // On the first edit, preserve every legacy price for all of its valid sizes.
  const sized = rows.some(row => /:(?:size|controls):/.test(row.tier_label))
  const expanded = sized ? rows : rows.flatMap(row =>
    /^quality:(low|medium|high)$/.test(row.tier_label) && row.per_request_price !== ''
      ? profile.sizes.map(item => ({ ...row, tier_label: tariffLabel(row.tier_label.slice(8) as ImageQuality, item.value) }))
      : [row])
  const label = tariffLabel(quality, size)
  const result = expanded.filter(row => row.tier_label !== label &&
    !(value !== '' && row.per_request_price === ''))
  if (value !== '' || result.length === 0) {
    result.push({ ...setImageQualityPrice([], quality, value)[0], tier_label: label })
  }
  const order = profile.sizes.flatMap(item => profile.qualities.map(q => tariffLabel(q, item.value)))
  return result.sort((a, b) => order.indexOf(a.tier_label) - order.indexOf(b.tier_label))
    .map((row, index) => ({ ...row, sort_order: index }))
}
