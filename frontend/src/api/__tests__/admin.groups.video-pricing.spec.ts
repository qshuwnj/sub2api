import { describe, expect, it, vi } from 'vitest'
import axios, { AxiosError } from 'axios'
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

  it('replays an expired-session group update as a JSON object with exact numeric video prices', async () => {
    const original = apiClient.defaults.adapter
    const refresh = vi.spyOn(axios, 'post').mockResolvedValue({ data: { code: 0, data: {
      access_token: 'fresh-access', refresh_token: 'fresh-refresh', expires_in: 3600, token_type: 'Bearer',
    } } })
    const requestBodies: unknown[] = []
    let requests = 0
    localStorage.setItem('auth_token', 'old-access')
    localStorage.setItem('refresh_token', 'old-refresh')
    localStorage.setItem('auth_user', JSON.stringify({ id: 1 }))
    apiClient.defaults.adapter = async config => {
      requests++
      requestBodies.push(config.data)
      if (requests === 1) {
        throw new AxiosError('expired', 'ERR_BAD_REQUEST', config, undefined, {
          status: 401, statusText: 'Unauthorized', headers: {}, config,
          data: { code: 401, message: 'expired' },
        })
      }
      return { config, status: 200, statusText: 'OK', headers: {}, data: {
        code: 0, data: { id: 1, model_pricing: [] },
      } }
    }
    try {
      await update(1, { model_pricing: [{
        platform: 'openai', models: ['google/veo3.1/text-to-video'], billing_mode: 'video',
        input_price: null, output_price: null, cache_write_price: null, cache_read_price: null,
        image_input_price: null, image_output_price: null, per_request_price: null, time_pricing: null,
        intervals: [{ tier_label: 'video:v1:fixture', per_request_price: '9999999999.12345678' }],
      } as never] })
      expect(requests).toBe(2)
      for (const body of requestBodies) {
        expect(typeof body).toBe('string')
        expect(JSON.parse(body as string)).toMatchObject({
          model_pricing: [{ intervals: [{ per_request_price: 9999999999.12345678 }] }],
        })
      }
      expect(refresh).toHaveBeenCalledOnce()
    } finally {
      apiClient.defaults.adapter = original
      refresh.mockRestore()
    }
  })
})
