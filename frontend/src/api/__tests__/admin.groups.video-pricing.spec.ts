import { describe, expect, it, vi } from 'vitest'
import { apiClient } from '../client'
import { getById, update } from '../admin/groups'

vi.mock('@/i18n', () => ({ getLocale: () => 'en' }))
describe('group API exact native-video DTO round trip', () => {
  it('keeps numeric wire decimals before axios parsing and emits an edited numeric amount without float projection', async () => {
    const original = apiClient.defaults.adapter
    let saved = ''
    apiClient.defaults.adapter = async config => {
      if (config.method === 'put') saved = config.data
      return { config, status: 200, statusText: 'OK', headers: {},
        data: '{"code":0,"data":{"id":1,"model_pricing":[{"billing_mode":"video","models":["model"],"intervals":[{"tier_label":"video:v1:fixture","per_request_price":9999999999.12345678}],"video_tariff":{"version":1,"cells":{}}}]}}' }
    }
    try {
      const group = await getById(1)
      expect(group.model_pricing?.[0].intervals[0].per_request_price).toBe('9999999999.12345678')
      await update(1, { model_pricing: group.model_pricing })
      expect(saved).toContain('"per_request_price":9999999999.12345678')
      expect(saved).not.toContain('"per_request_price":"')
    } finally {
      apiClient.defaults.adapter = original
    }
  })
})
