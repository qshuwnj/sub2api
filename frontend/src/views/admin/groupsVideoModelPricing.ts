export const grokVideoPriceResolutions = [
  { key: '480p', label: '480p' },
  { key: '720p', label: '720p' },
  { key: '1080p', label: '1080p' }
] as const

export const grokVideoPriceFamilies = [
  { key: 'grok-imagine-video', label: 'grok-imagine-video' },
  { key: 'grok-imagine-video-1.5', label: 'grok-imagine-video-1.5' },
  { key: 'google/gemini-omni-1.1-flash/text-to-video', label: 'google/gemini-omni-1.1-flash/text-to-video' },
  { key: 'minimax/h3-developer/text-to-video', label: 'minimax/h3-developer/text-to-video' },
  { key: 'xai/grok-imagine-video/text-to-video', label: 'xai/grok-imagine-video/text-to-video' }
] as const

export function videoPriceResolutionsForModel(model: string) {
  switch (model) {
    case 'google/gemini-omni-1.1-flash/text-to-video':
      return ['360p', '720p', '1080p', '4k'].map((key) => ({ key, label: key }))
    case 'minimax/h3-developer/text-to-video':
      return ['480p', '768p', '1440p-sr', '4k-sr'].map((key) => ({ key, label: key }))
    case 'xai/grok-imagine-video/text-to-video':
      return ['480p', '720p'].map((key) => ({ key, label: key }))
    default:
      return grokVideoPriceResolutions
  }
}

export type VideoModelPrices = Record<string, Record<string, number>>
export type VideoModelPricesForm = Record<string, Record<string, number | string | null>>

function normalizeFamily(value: string): string {
  return value.trim().toLowerCase()
}

function normalizePrice(value: unknown): number | null {
  if (value === null || value === undefined || value === '') return null
  const price = Number(value)
  return Number.isFinite(price) && price >= 0 ? price : null
}

function emptyTiers(model: string): Record<string, number | string | null> {
  return Object.fromEntries(videoPriceResolutionsForModel(model).map(({ key }) => [key, null]))
}

// Keep unknown families from an existing group so a future backend catalog is
// not silently discarded when an operator edits another group setting.
export function createVideoModelPricesForm(
  prices?: VideoModelPrices | null
): VideoModelPricesForm {
  const form: VideoModelPricesForm = {}

  for (const [rawFamily, rawTiers] of Object.entries(prices ?? {})) {
    const family = normalizeFamily(rawFamily)
    if (!family || !rawTiers || typeof rawTiers !== 'object') continue
    form[family] = emptyTiers(family)
    for (const [rawResolution, rawPrice] of Object.entries(rawTiers)) {
      const price = normalizePrice(rawPrice)
      if (price !== null) form[family][rawResolution.trim().toLowerCase()] = price
    }
  }

  for (const { key } of grokVideoPriceFamilies) {
    form[key] ??= emptyTiers(key)
  }
  return form
}

export function serializeVideoModelPrices(form: VideoModelPricesForm): VideoModelPrices {
  const result: VideoModelPrices = {}
  for (const [rawFamily, tiers] of Object.entries(form)) {
    const family = normalizeFamily(rawFamily)
    if (!family || !tiers || typeof tiers !== 'object') continue

    const normalizedTiers: Record<string, number> = {}
    for (const [rawResolution, rawPrice] of Object.entries(tiers)) {
      const resolution = rawResolution.trim().toLowerCase()
      const price = normalizePrice(rawPrice)
      if (resolution && price !== null) normalizedTiers[resolution] = price
    }
    if (Object.keys(normalizedTiers).length > 0) result[family] = normalizedTiers
  }
  return result
}

export function videoModelPriceFamilyRows(form: VideoModelPricesForm) {
  const known = new Set<string>(grokVideoPriceFamilies.map(({ key }) => key))
  const extra = Object.keys(form)
    .map(normalizeFamily)
    .filter((family) => family && !known.has(family))
    .sort()
    .map((key) => ({ key, label: key }))
  return [...grokVideoPriceFamilies, ...extra]
}
