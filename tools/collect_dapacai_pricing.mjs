import { mkdir, writeFile } from 'node:fs/promises'
import { resolve } from 'node:path'
import { buildDapacaiPricingDraft, DAPACAI_PRICING_SOURCE } from './dapacai_pricing.mjs'

const [supplierGroup, outputDirectory] = process.argv.slice(2)
if (!supplierGroup || !outputDirectory) throw new Error('Usage: node tools/collect_dapacai_pricing.mjs <verified-supplier-group> <output-directory>')
const response = await fetch(DAPACAI_PRICING_SOURCE, { redirect: 'error', signal: AbortSignal.timeout(30000) })
if (!response.ok) throw new Error(`Supplier pricing HTTP ${response.status}`)
const snapshot = await response.json()
const draft = buildDapacaiPricingDraft(snapshot, { supplierGroup, checkedAt: new Date().toISOString() })
const target = resolve(outputDirectory)
await mkdir(target, { recursive: true })
await writeFile(resolve(target, 'upstream-pricing.json'), JSON.stringify(snapshot, null, 2) + '\n')
await writeFile(resolve(target, 'group-pricing-draft.json'), JSON.stringify(draft, null, 2) + '\n')
console.log(`Prepared ${draft.group_patch.model_pricing.length} exact USD cards, markup 1.2, FX 1. Supplier group ${supplierGroup}: ${draft.policy.supplier_group_ratio}.`)
console.log('Draft only. Verify the upstream account group before applying. No production settings changed.')
