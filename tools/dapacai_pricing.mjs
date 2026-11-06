import assert from 'node:assert/strict'

export const DAPACAI_PRICING_SOURCE = 'https://api.dapacai.com/api/pricing'
export const DAPACAI_MARKUP = 1.2
const videoModels = [
  'seedance-2.0', 'seedance-2.0-plus', 'seedance-2.5-plus',
  ...Array.from({ length: 7 }, (_, i) => `seedance-2.5-plus-${i + 1}`),
  'wan-3.0',
]
const imageModel = 'gpt-image-2.5-cdn'
const textModels = ['gpt-5.6-sol', 'gpt-6.1-sol']
const supported = new Set([...videoModels, imageModel, ...textModels])
const money = value => {
  assert.ok(typeof value === 'number' && Number.isFinite(value) && value >= 0, 'Invalid upstream money')
  return value
}
const multiply = (a, b) => Number((money(a) * money(b)).toFixed(10))

// Deliberately not a general expression interpreter. Changes in supplier billing
// must be reviewed; never fall back from an unknown expression to ModelPrice.
function priceCard(row, ratio) {
  let mode = row.model_name === imageModel ? 'image' : 'per_request'
  let price
  if (row.billing_mode === 'tiered_expr') {
    const match = /^tier\("base",\s*u\("duration"\)\s*\*\s*(\d+(?:\.\d+)?)\)$/.exec(row.billing_expr ?? '')
    assert.ok(match && row.billing_usage_schema?.duration?.unit === 'second', `Unverified billing expression: ${row.model_name}`)
    mode = 'video'
    price = Number(match[1])
  } else {
    assert.ok(row.quota_type === 1 && (!row.billing_mode || row.billing_mode === 'ratio'),
      `Unverified billing mode: ${row.model_name}`)
    price = money(row.model_price)
  }
  return { platform: 'openai', models: [row.model_name], billing_mode: mode,
    per_request_price: multiply(price, ratio) }
}

export function buildDapacaiPricingDraft(snapshot, { supplierGroup, checkedAt }) {
  assert.equal(snapshot.success, true, 'Supplier pricing request failed')
  assert.ok(supplierGroup && Object.hasOwn(snapshot.group_ratio ?? {}, supplierGroup), 'Select a published supplier group explicitly')
  assert.ok(Number.isFinite(Date.parse(checkedAt)), 'Missing verification timestamp')
  const ratio = money(snapshot.group_ratio[supplierGroup])
  const seen = new Set()
  const evidence = []
  const model_pricing = []
  for (const row of snapshot.data ?? []) {
    const model = row.model_name
    if (!supported.has(model)) continue
    assert.ok(!seen.has(model), `Duplicate supplier model: ${model}`)
    seen.add(model)
    assert.ok(row.enable_groups?.includes(supplierGroup), `Model unavailable in supplier group: ${model}`)
    assert.ok(!row.billing_plugin_variants?.length && !row.plugin_billing_expr,
      `Provider-specific pricing needs review: ${model}`)
    let card
    if (textModels.includes(model)) {
      assert.equal(row.quota_type, 0, `Expected token pricing: ${model}`)
      // Supplier ModelRatio uses USD/1M; gateway JSON cards store USD/token.
      const input = multiply(money(row.model_ratio), 2) / 1_000_000
      card = { platform: 'openai', models: [model], billing_mode: 'token',
        input_price: multiply(input, ratio),
        output_price: multiply(multiply(input, money(row.completion_ratio)), ratio) }
      if (row.cache_ratio !== undefined) card.cache_read_price = multiply(multiply(input, money(row.cache_ratio)), ratio)
      if (row.create_cache_ratio !== undefined) card.cache_write_price = multiply(multiply(input, money(row.create_cache_ratio)), ratio)
    } else {
      card = priceCard(row, ratio)
    }
    model_pricing.push(card)
    evidence.push({ model, source_row: structuredClone(row), upstream_card_usd: structuredClone(card) })
  }
  assert.deepEqual([...seen].sort(), [...supported].sort(), 'Incomplete upstream price list; no partial import')
  return {
    policy: { provider: 'dapacai', currency: 'USD', markup: DAPACAI_MARKUP, exchange: 1,
      source: DAPACAI_PRICING_SOURCE, checked_at: checkedAt, pricing_version: snapshot.pricing_version,
      supplier_group: supplierGroup, supplier_group_ratio: ratio,
      requires_account_group_verification: true },
    group_patch: {
      rate_multiplier: DAPACAI_MARKUP, peak_rate_enabled: false,
      image_count_billing: true, image_rate_independent: true, image_rate_multiplier: DAPACAI_MARKUP,
      video_rate_independent: true, video_rate_multiplier: DAPACAI_MARKUP,
      model_pricing,
    },
    evidence,
    excluded: [{ model: 'minimax-h3', reason: 'Disabled in supplier catalog' }],
  }
}

// Pure targeted patch builder: keep all unrelated prices, model allowlists,
// accounts and bindings. Applying this result is a separate authorized action.
export function mergeDapacaiPricingDraft(group, draft, expectedName) {
  assert.ok(expectedName && group.name === expectedName, 'Wrong target group')
  assert.equal(group.platform, 'openai', 'Wrong target platform')
  assert.equal(draft.policy?.provider, 'dapacai')
  assert.equal(draft.policy.currency, 'USD')
  assert.equal(draft.policy.exchange, 1, 'FX is not authorized for this group')
  assert.equal(draft.policy.markup, DAPACAI_MARKUP)
  for (const field of ['rate_multiplier', 'image_rate_multiplier', 'video_rate_multiplier']) {
    assert.equal(draft.group_patch[field], DAPACAI_MARKUP, 'Markup must apply exactly once')
  }
  const oldCards = structuredClone(group.model_pricing ?? []).map(card => ({
    ...card, models: card.models.filter(model => !supported.has(model)),
  })).filter(card => card.models.length)
  const video_model_prices = Object.fromEntries(Object.entries(group.video_model_prices ?? {})
    .filter(([model]) => !supported.has(model)))
  return { ...structuredClone(draft.group_patch), video_model_prices,
    model_pricing: [...oldCards, ...structuredClone(draft.group_patch.model_pricing)] }
}
