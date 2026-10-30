import profilesJSON from './atlasVideoTariffProfiles.json'
import type { ChannelModelPricing, VideoTariffMetadata } from '@/api/admin/channels'

interface NativeProfile {
  mode: string
  contractRevision: string
  variants: { controls: Record<string, unknown>; durations: (number | string)[] }[]
}
const profiles = profilesJSON as Record<string, NativeProfile>
const policy = 'fixed-customer-price-platform-variance-v1'
const source = 'https://api.atlascloud.ai/api/v1/model/calculate'
const exactJSON = JSON as typeof JSON & { rawJSON?: (text: string) => unknown }
const parseWithSource = JSON.parse as (text: string, reviver: (
  this: Record<string, unknown>, key: string, value: unknown, context?: { source?: string },
) => unknown) => any
function canonical(value: unknown): string {
  if (Array.isArray(value)) return '[' + value.map(canonical).join(',') + ']'
  if (value && typeof value === 'object') {
    return '{' + Object.entries(value).sort(([a], [b]) => a.localeCompare(b))
      .map(([key, child]) => JSON.stringify(key) + ':' + canonical(child)).join(',') + '}'
  }
  return JSON.stringify(value)
}
function keyFor(model: string, mode: string, controls: Record<string, unknown>) {
  const bytes = new TextEncoder().encode(canonical([model, mode, controls]))
  return 'video:v1:' + btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}
export function nativeVideoCells(model: string) {
  const profile = profiles[model]
  if (!profile) return []
  return profile.variants.flatMap(variant => variant.durations.map(duration => {
    const controls = { ...variant.controls, duration }
    return { key: keyFor(model, profile.mode, controls), controls }
  }))
}
export function exactVideoPrice(value: unknown): string {
  const text = typeof value === 'string' ? value : String(value)
  if (!/^(?:0|[1-9]\d{0,9})(?:\.\d{1,8})?$/.test(text)) {
    throw new Error('Invalid video job price: use a decimal with at most eight places below 10000000000')
  }
  return text
}
export function parseVideoPricingJSON(text: string): any {
  return parseWithSource(text, function (key, value, context) {
    if (key === 'per_request_price' && typeof this.tier_label === 'string' && this.tier_label.startsWith('video:') &&
        typeof value === 'number') {
      if (!context?.source) throw new Error('This browser cannot safely edit exact video prices')
      return context.source
    }
    return value
  })
}
export function stringifyVideoPricingJSON(value: unknown): string {
  if (typeof value === 'string') {
    return stringifyVideoPricingJSON(parseVideoPricingJSON(value))
  }
  return JSON.stringify(value, function (key, child) {
    if (key === 'per_request_price' && typeof this.tier_label === 'string' && this.tier_label.startsWith('video:')) {
      if (!exactJSON.rawJSON) throw new Error('This browser cannot safely save exact video prices')
      return exactJSON.rawJSON(exactVideoPrice(child))
    }
    return child
  })
}
export function validateNativeVideoCard(value: unknown): string | null {
  try {
    const card = value as ChannelModelPricing
    const model = card.models?.[0]
    const profile = profiles[model]
    if (!profile || card.models.length !== 1 || card.platform && card.platform !== 'openai' ||
        card.billing_mode !== 'video' || !profile.variants.length) throw new Error('An exact supported video endpoint is required')
    const metadata = card.video_tariff
    if (!metadata || metadata.version !== 1 || metadata.contract_revision !== profile.contractRevision) {
      throw new Error('Video contract revision is missing or differs from the verified endpoint')
    }
    for (const field of ['input_price', 'output_price', 'cache_write_price', 'cache_write_1h_price',
      'cache_read_price', 'image_input_price', 'image_output_price', 'per_request_price',
      'fast_multiplier', 'flex_multiplier', 'reasoning_effort_multipliers', 'time_pricing'] as const) {
      const current: unknown = card[field]
      if (current != null && current !== '' && !(field === 'time_pricing' && !(current as { periods?: unknown[] }).periods?.length)) {
        throw new Error('Native video cards cannot contain fallback prices or modifiers')
      }
    }
    const expected = new Set(nativeVideoCells(model).map(cell => cell.key))
    const seen = new Set<string>()
    for (const row of card.intervals || []) {
      if (!expected.has(row.tier_label) || seen.has(row.tier_label)) throw new Error('Duplicate, unsupported or mistyped native video cell')
      seen.add(row.tier_label)
      const price = exactVideoPrice(row.per_request_price)
      const evidence = metadata.cells[row.tier_label]
      if (!evidence || evidence.source !== source || !Number.isFinite(Date.parse(evidence.checked_at)) ||
          !/^(?:0|[1-9]\d{0,11})(?:\.\d{1,18})?$/.test(evidence.upstream_price) ||
          Number(evidence.upstream_price) <= 0 || typeof evidence.supplier_estimated !== 'boolean' ||
          evidence.policy !== policy || (Number(price) === 0) !== (evidence.explicit_free === true)) {
        throw new Error('Video provenance is invalid; zero requires explicit free intent')
      }
      for (const field of ['input_price', 'output_price', 'cache_write_price', 'cache_write_1h_price',
        'cache_read_price', 'input_multiplier', 'output_multiplier', 'cache_write_multiplier', 'cache_read_multiplier'] as const) {
        const current: unknown = row[field]
        if (current != null && current !== '') throw new Error('Video cells cannot contain token prices or multipliers')
      }
    }
    if (seen.size !== expected.size || Object.keys(metadata.cells).length !== seen.size) {
      throw new Error(`Incomplete video grid: ${seen.size}/${expected.size}`)
    }
    return null
  } catch (error) {
    return error instanceof Error ? error.message : 'Invalid video pricing card'
  }
}
export function validateNativeVideoCardUpdate(card: ChannelModelPricing, existing: ChannelModelPricing[]): string | null {
  const error = validateNativeVideoCard(card)
  if (!error) return null
  // Stored invalid cards stay blocked at quote time, but do not prevent an unrelated repair.
  return existing.some(prior => canonical(prior) === canonical(card)) ? null : error
}

function migrateReviewedKlingDefaults(prior: ChannelModelPricing): ChannelModelPricing | null {
  const migrations: Record<string, { revision: string; field: string }> = {
    'kwaivgi/kling-v2.5-turbo-pro/text-to-video': {
      revision: 'cb4e9122308e2e1d0913c9de7f18a9a753bf14477c541c67505e383df3e8e40a', field: 'guidance_scale',
    },
    'kwaivgi/kling-v2.6-pro/text-to-video': {
      revision: '4f4ea4b379e006c25c1b636801f7ea48b141f806aa985fd0f214809252d4cd4b', field: 'cfg_scale',
    },
  }
  const model = prior.models[0]
  const migration = migrations[model]
  if (!migration || prior.video_tariff?.contract_revision !== migration.revision) return null
  try {
    if (Object.keys(prior.video_tariff.cells).length !== prior.intervals.length) return null
    const cells: NonNullable<ChannelModelPricing['video_tariff']>['cells'] = {}
    const intervals = prior.intervals.map(row => {
      const encoded = row.tier_label.slice('video:v1:'.length).replace(/-/g, '+').replace(/_/g, '/')
      const [endpoint, mode, controls] = JSON.parse(atob(encoded))
      if (!row.tier_label.startsWith('video:v1:') || endpoint !== model || mode !== 't2v' ||
          controls[migration.field] !== 0.5) throw new Error('Unreviewed prior controls')
      delete controls[migration.field]
      const key = keyFor(model, mode, controls)
      if (cells[key]) throw new Error('Ambiguous prior cells')
      cells[key] = { ...prior.video_tariff!.cells[row.tier_label] }
      return { ...row, tier_label: key }
    })
    const migrated = { ...prior, intervals, video_tariff: {
      ...prior.video_tariff, version: 1 as const, contract_revision: profiles[model].contractRevision, cells,
    } }
    return validateNativeVideoCard(migrated) ? null : migrated
  } catch { return null }
}
export function importNativeVideoDraft(text: string, existing: ChannelModelPricing[]): ChannelModelPricing[] {
  const document = parseVideoPricingJSON(text)
  const cards = Array.isArray(document) ? document : document.model_pricing
  if (!Array.isArray(cards)) throw new Error('Import must contain model_pricing')
  const native = cards.filter(card => card.video_tariff)
  if (!native.length) throw new Error('No verified native video cards in this draft')
  const result = [...existing]
  const importedModels = new Set<string>()
  for (const card of native) {
    const error = validateNativeVideoCard(card)
    if (error) throw new Error(error)
    const model = card.models[0]
    if (importedModels.has(model.toLowerCase())) throw new Error(`Overlapping model card: ${model}`)
    importedModels.add(model.toLowerCase())
    const exactExisting = result.filter(prior => prior.billing_mode === 'video' && prior.models.length === 1 &&
      prior.models[0].toLowerCase() === model.toLowerCase() && prior.video_tariff)
    if (exactExisting.length > 1) throw new Error(`Overlapping existing video cards: ${model}`)
    if (exactExisting.length === 1) {
      const original = exactExisting[0]
      const prior = validateNativeVideoCard(original) ? migrateReviewedKlingDefaults(original) || original : original
      const priorError = validateNativeVideoCard(prior)
      if (priorError) throw new Error(`Existing video card is invalid: ${priorError}`)
      const oldPrices = new Map(prior.intervals.map((row: ChannelModelPricing['intervals'][number]) => [row.tier_label, exactVideoPrice(row.per_request_price)]))
      const incomingPrices = new Map(card.intervals.map((row: ChannelModelPricing['intervals'][number]) => [row.tier_label, exactVideoPrice(row.per_request_price)]))
      if (oldPrices.size !== incomingPrices.size || [...oldPrices.keys()].some(key => !incomingPrices.has(key))) {
        throw new Error(`Conflicting video tariff grid: ${model}`)
      }
      const refreshed = {
        ...prior,
        video_tariff: copyVideoMetadata(card.video_tariff),
        intervals: prior.intervals.map((row: ChannelModelPricing['intervals'][number]) => ({ ...row })),
      }
      for (const row of refreshed.intervals as ChannelModelPricing['intervals']) {
        const evidence = refreshed.video_tariff?.cells[row.tier_label]
        if (evidence) {
          if (Number(exactVideoPrice(row.per_request_price)) === 0) evidence.explicit_free = true
          else delete evidence.explicit_free
        }
      }
      const refreshedError = validateNativeVideoCard(refreshed)
      if (refreshedError) throw new Error(`Refreshed video tariff is invalid: ${refreshedError}`)
      result[result.indexOf(original)] = refreshed
      continue
    }
    if (result.some(prior => prior.models.some(pattern => {
      const normalized = pattern.toLowerCase()
      return normalized === model.toLowerCase() ||
        normalized.endsWith('*') && model.toLowerCase().startsWith(normalized.slice(0, -1))
    }))) throw new Error(`Overlapping model card: ${model}`)
    result.push(card)
  }
  return result
}
export function newNativeVideoAdditions(
  merged: ChannelModelPricing[],
  existing: ChannelModelPricing[],
): ChannelModelPricing[] {
  const existingVideoModels = new Set(existing
    .filter(card => card.video_tariff && card.models?.[0])
    .map(card => card.models[0].toLowerCase()))
  return merged.filter(card => card.video_tariff && card.models?.[0] &&
    !existingVideoModels.has(card.models[0].toLowerCase()))
}
export function copyVideoMetadata(metadata?: VideoTariffMetadata): VideoTariffMetadata | undefined {
  return metadata ? { ...metadata, cells: Object.fromEntries(Object.entries(metadata.cells).map(([key, cell]) => [key, { ...cell }])) } : undefined
}
