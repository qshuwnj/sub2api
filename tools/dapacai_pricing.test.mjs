import assert from 'node:assert/strict'
import test from 'node:test'
import { buildDapacaiPricingDraft, mergeDapacaiPricingDraft } from './dapacai_pricing.mjs'

function snapshot(ratio = 1) {
  const names = ['seedance-2.0', 'seedance-2.0-plus', 'seedance-2.5-plus',
    ...Array.from({ length: 7 }, (_, i) => `seedance-2.5-plus-${i + 1}`), 'wan-3.0', 'gpt-image-2.5-cdn']
  const data = names.map(model_name => ({
    model_name, quota_type: 1, model_price: model_name === 'gpt-image-2.5-cdn' ? 0.1 : 1.3,
    enable_groups: ['default'], billing_usage_schema: { duration: { type: 'number', unit: 'second' } },
  }))
  Object.assign(data.find(row => row.model_name === 'seedance-2.5-plus-7'), {
    model_price: 4.1, billing_mode: 'tiered_expr', billing_expr: 'tier("base", u("duration") * 0.41)',
  })
  for (const model_name of ['gpt-5.6-sol', 'gpt-6.1-sol']) data.push({
    model_name, quota_type: 0, model_ratio: 2.5, completion_ratio: 6,
    cache_ratio: 0.1, create_cache_ratio: 1.25, enable_groups: ['default'],
  })
  return { success: true, pricing_version: 'fixture', group_ratio: { default: ratio }, data }
}
const options = { supplierGroup: 'default', checkedAt: '2026-11-06T13:00:00Z' }
const cardFor = (draft, model) => draft.group_patch.model_pricing.find(card => card.models.includes(model))

test('raw USD cards plus 1.2 markup, no FX or baked-in/double markup', () => {
  const draft = buildDapacaiPricingDraft(snapshot(), options)
  assert.equal(draft.policy.exchange, 1)
  assert.equal(draft.group_patch.rate_multiplier, 1.2)
  assert.equal(draft.group_patch.video_rate_multiplier, 1.2)
  assert.equal(draft.group_patch.image_rate_multiplier, 1.2)
  const image = cardFor(draft, 'gpt-image-2.5-cdn')
  assert.equal(image.per_request_price, 0.1)
  assert.equal(Number((image.per_request_price * draft.group_patch.image_rate_multiplier).toFixed(10)), 0.12)
  assert.equal(draft.group_patch.model_pricing.length, 14)
})

test('fixed ModelPrice is per job; duration schema alone does not authorize seconds multiplication', () => {
  const draft = buildDapacaiPricingDraft(snapshot(), options)
  const fixed = cardFor(draft, 'seedance-2.0')
  assert.equal(fixed.billing_mode, 'per_request')
  assert.equal(fixed.per_request_price, 1.3)
  const perSecond = cardFor(draft, 'seedance-2.5-plus-7')
  assert.equal(perSecond.billing_mode, 'video')
  assert.equal(perSecond.per_request_price, 0.41)
  assert.equal(Number((perSecond.per_request_price * 15 * 1.2).toFixed(10)), 7.38)
})

test('supplier group discount is explicit and not mistaken for a currency conversion', () => {
  const draft = buildDapacaiPricingDraft(snapshot(0.3), options)
  assert.equal(draft.policy.supplier_group_ratio, 0.3)
  assert.equal(cardFor(draft, 'seedance-2.0').per_request_price, 0.39)
  assert.equal(cardFor(draft, 'gpt-image-2.5-cdn').per_request_price, 0.03)
  assert.equal(cardFor(draft, 'seedance-2.5-plus-7').per_request_price, 0.123)
  const token = cardFor(draft, 'gpt-5.6-sol')
  assert.equal(token.input_price, 0.0000015)
  assert.equal(token.output_price, 0.000009)
  assert.equal(token.cache_read_price, 0.00000015)
})

test('incomplete prices, unknown expressions, duplicates and unsupported groups fail closed', () => {
  const attempts = [
    s => { s.data.pop() },
    s => { s.data.push(structuredClone(s.data[0])) },
    s => { s.data[0].model_price = NaN },
    s => { s.data.find(row => row.billing_expr).billing_expr = 'tier("base", fixed(1))' },
    s => { s.group_ratio.default = -1 },
    s => { s.data[0].enable_groups = ['vip'] },
    s => { s.data[0].billing_plugin_variants = [{ billing_mode: 'ratio' }] },
  ]
  for (const mutate of attempts) {
    const s = snapshot()
    mutate(s)
    assert.throws(() => buildDapacaiPricingDraft(s, options))
  }
  assert.throws(() => buildDapacaiPricingDraft(snapshot(), { ...options, supplierGroup: 'other' }))
})

test('targeted merge preserves old provider cards, settings and original objects', () => {
  const group = { name: 'new-test-group', platform: 'openai', rate_multiplier: 0.4,
    model_allowlist: ['gpt-image-2.5-cdn'], account_ids: [72],
    video_model_prices: { 'seedance-2.0': { native: 99 },
      'bytedance/seedance-2.0/text-to-video': { '480p': 8.33232 } },
    model_pricing: [{ models: ['atlas-old', 'seedance-2.0'], billing_mode: 'per_request', per_request_price: 42 }] }
  const before = structuredClone(group)
  const draft = buildDapacaiPricingDraft(snapshot(), options)
  const patch = mergeDapacaiPricingDraft(group, draft, 'new-test-group')
  assert.deepEqual(group, before)
  assert.deepEqual(patch.model_pricing[0], { models: ['atlas-old'], billing_mode: 'per_request', per_request_price: 42 })
  assert.deepEqual(patch.video_model_prices, { 'bytedance/seedance-2.0/text-to-video': { '480p': 8.33232 } })
  assert.equal(patch.account_ids, undefined)
  assert.equal(patch.model_allowlist, undefined)
  assert.equal(patch.rate_multiplier, 1.2)
  assert.throws(() => mergeDapacaiPricingDraft(group, draft, 'old-atlas-group'))
  draft.policy.exchange = 6.9436
  assert.throws(() => mergeDapacaiPricingDraft(group, draft, 'new-test-group'))
})
