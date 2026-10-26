import { createHash } from 'node:crypto'
import { readFile, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const profilesPath = resolve(root, 'frontend/src/components/admin/channel/atlasVideoTariffProfiles.json')
const draftPath = resolve(root, 'frontend/src/components/admin/channel/atlasSeedanceR2VTariffDraft.json')

const ratios = ['16:9', '4:3', '1:1', '3:4', '9:16', '21:9']
const standardControls = {
  bitrate_mode: 'standard',
  generate_audio: true,
  seed: -1,
  watermark: false,
  return_last_frame: false,
}
const seedance25Controls = {
  generate_audio: true,
  watermark: false,
  return_last_frame: false,
  output_format: 'mp4',
  draft: false,
}
const models = [
  {
    id: 'bytedance/seedance-2.0/reference-to-video',
    revision: 'dd4d199e9175d171614a71dfd603fe106eabc39b040f098af923832eb52ff008',
    resolutions: ['480p', '720p', '720p-SR', '1080p', '1080p-SR', '1440p-SR', '4k'],
    durations: 15,
    rawPerSecond: {
      '480p': '0.08999424',
      '720p': '0.1935',
      '720p-sr': '0.162',
      '1080p': '0.4355',
      '1080p-sr': '0.3484',
      '1440p-sr': '0.6193',
      '4k': '0.9722',
    },
    controls: standardControls,
  },
  {
    id: 'bytedance/seedance-2.0-mini/reference-to-video',
    revision: 'c029126f05deff62bcc2ab5f08ca5afc78e75544742f3eef7ec5836dba0134b2',
    resolutions: ['480p', '720p', '720p-SR', '1080p-SR', '1440p-SR'],
    durations: 15,
    rawPerSecond: {
      '480p': '0.04499712',
      '720p': '0.0968',
      '720p-sr': '0.081',
      '1080p-sr': '0.1742',
      '1440p-sr': '0.3097',
    },
    controls: standardControls,
  },
  {
    id: 'bytedance/seedance-2.0-fast/reference-to-video',
    revision: '01d53fc60069f7156c7d48a72c567ac84ca70b766d7fda14c60cf9981258cf72',
    resolutions: ['480p', '720p', '720p-SR', '1080p-SR', '1440p-SR'],
    durations: 15,
    rawPerSecond: {
      '480p': '0.071995392',
      '720p': '0.1548',
      '720p-sr': '0.1296',
      '1080p-sr': '0.2787',
      '1440p-sr': '0.4955',
    },
    controls: standardControls,
  },
  {
    id: 'bytedance/seedance-2.5/reference-to-video',
    revision: 'e2252525f721553959239b449779799516c41f935f2849daae5282bca3ced230',
    resolutions: ['480p', '720p', '720p-sr', '720p-esr', '1080p', '1080p-sr', '1080p-esr', '1080p-esr & 60fps', '1440p-sr', '1440p-esr', '4k-esr'],
    durations: 30,
    rawPerSecond: {
      '480p': '0.13971204',
      '720p': '0.3005',
      '720p-sr': '0.2193',
      '720p-esr': '0.2515',
      '1080p': '0.5914',
      '1080p-sr': '0.4056',
      '1080p-esr': '0.4507',
      '1080p-esr & 60fps': '0.4958',
      '1440p-sr': '0.7',
      '1440p-esr': '0.7602',
      '4k-esr': '1.7006',
    },
    controls: seedance25Controls,
  },
]

function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  if (value && typeof value === 'object') {
    return `{${Object.entries(value).sort(([a], [b]) => a.localeCompare(b))
      .map(([key, child]) => `${JSON.stringify(key)}:${canonical(child)}`).join(',')}}`
  }
  return JSON.stringify(value)
}

function videoKey(model, controls) {
  const encoded = Buffer.from(canonical([model, 'r2v', controls])).toString('base64url')
  return `video:v1:${encoded}`
}

function parseScaled8(value) {
  const [whole, fraction = ''] = value.split('.')
  return BigInt(whole) * 100000000n + BigInt((fraction + '00000000').slice(0, 8))
}

function roundDiv(numerator, denominator) {
  return (numerator + denominator / 2n) / denominator
}

function scaledToString(value) {
  const whole = value / 100000000n
  const fraction = String(value % 100000000n).padStart(8, '0')
  return `${whole}.${fraction}`
}

function numeric(value) {
  return Number(scaledToString(value))
}

function profileFor(model) {
  const controlBase = model.controls === standardControls ? standardControls : seedance25Controls
  const variants = []
  for (const ratio of ratios) {
    for (const resolution of model.resolutions) {
      variants.push({
        controls: { ...controlBase, ratio, resolution },
        durations: Array.from({ length: model.durations - 3 }, (_, index) => index + 4),
      })
    }
  }
  const keys = variants.flatMap(variant => variant.durations.map(duration => videoKey(model.id, { ...variant.controls, duration }))).sort()
  return {
    mode: 'r2v',
    contractRevision: model.revision,
    variants,
    keysSHA256: createHash('sha256').update(keys.join('\n')).digest('hex'),
  }
}

function draftCard(model, profile) {
  const factorNumerator = 833232n
  const factorDenominator = 100000n
  const intervals = []
  const cells = {}
  for (const variant of profile.variants) {
    for (const duration of variant.durations) {
      const controls = { ...variant.controls, duration }
      const key = videoKey(model.id, controls)
      const raw = parseScaled8(model.rawPerSecond[variant.controls.resolution.toLowerCase()])
      const upstream = raw * BigInt(duration)
      const retail = roundDiv(upstream * factorNumerator, factorDenominator)
      intervals.push({ tier_label: key, per_request_price: numeric(retail) })
      cells[key] = {
        source: 'https://api.atlascloud.ai/api/v1/model/calculate',
        checked_at: '2026-10-26T00:00:00Z',
        upstream_price: scaledToString(upstream),
        supplier_estimated: true,
        policy: 'fixed-customer-price-platform-variance-v1',
      }
    }
  }
  return {
    platform: 'openai',
    models: [model.id],
    billing_mode: 'video',
    intervals,
    video_tariff: {
      version: 1,
      contract_revision: profile.contractRevision,
      cells,
    },
  }
}

const profiles = JSON.parse(await readFile(profilesPath, 'utf8'))
const draft = []
for (const model of models) {
  const profile = profileFor(model)
  profiles[model.id] = profile
  draft.push(draftCard(model, profile))
}
await writeFile(profilesPath, `${JSON.stringify(profiles, null, 2)}\n`)
await writeFile(draftPath, `${JSON.stringify({ model_pricing: draft }, null, 2)}\n`)
