<template>
  <div class="mt-3 min-w-0" data-testid="native-video-prices">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <label class="text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.channels.form.nativeVideoPrices') }} <span>USD/job</span></label>
      <input v-model="search" type="search" class="input max-w-xs text-sm" :aria-label="t('admin.channels.form.nativeVideoSearch')" :placeholder="t('admin.channels.form.nativeVideoSearch')" @input="page = 0" />
    </div>
    <p v-if="error" role="alert" class="mt-2 break-words text-xs text-red-500">{{ error }}</p>
    <div class="mt-2 max-h-96 overflow-auto">
      <table class="w-full table-fixed text-left text-xs">
        <thead><tr class="text-gray-500 dark:text-gray-400">
          <th class="w-1/2 p-2">{{ t('admin.channels.form.nativeVideoControls') }}</th>
          <th class="p-2">USD/job</th>
          <th class="w-20 p-2">{{ t('admin.channels.form.nativeVideoFree') }}</th>
        </tr></thead>
        <tbody>
          <tr v-for="row in visible" :key="row.tier_label" class="border-t border-gray-200 dark:border-dark-600">
            <td class="break-words p-2 align-top">
              {{ label(row.tier_label) }}
              <details class="mt-1 text-gray-500 dark:text-gray-400">
                <summary>{{ t('admin.channels.form.nativeVideoEvidence') }}</summary>
                <div class="mt-1 break-all">{{ entry.video_tariff?.cells[row.tier_label]?.checked_at }}</div>
                <div>{{ entry.video_tariff?.cells[row.tier_label]?.upstream_price }} USD · {{ entry.video_tariff?.cells[row.tier_label]?.supplier_estimated ? t('admin.channels.form.nativeVideoEstimated') : t('admin.channels.form.nativeVideoVerified') }}</div>
              </details>
            </td>
            <td class="p-2 align-top">
              <input :value="row.per_request_price" type="text" inputmode="decimal" class="input w-full min-w-0 text-sm tabular-nums"
                data-video-price :aria-label="`${label(row.tier_label)} USD/job`"
                @input="edit(row.tier_label, ($event.target as HTMLInputElement).value)" />
            </td>
            <td class="p-2 align-top">
              <input type="checkbox" :checked="entry.video_tariff?.cells[row.tier_label]?.explicit_free === true"
                :aria-label="`${label(row.tier_label)} ${t('admin.channels.form.nativeVideoFree')}`"
                @change="setFree(row.tier_label, ($event.target as HTMLInputElement).checked)" />
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div class="mt-2 flex items-center justify-end gap-2 text-xs text-gray-500">
      <span>{{ page + 1 }} / {{ Math.max(1, Math.ceil(filtered.length / pageSize)) }}</span>
      <button type="button" class="rounded p-1 disabled:opacity-40" :disabled="page === 0" :title="t('admin.channels.form.nativeVideoPrevious')" :aria-label="t('admin.channels.form.nativeVideoPrevious')" @click="page--"><Icon name="chevronLeft" size="sm" /></button>
      <button type="button" class="rounded p-1 disabled:opacity-40" :disabled="(page + 1) * pageSize >= filtered.length" :title="t('common.next')" :aria-label="t('common.next')" @click="page++"><Icon name="chevronRight" size="sm" /></button>
    </div>
  </div>
</template>
<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { PricingFormEntry } from './types'
import { nativeVideoCells, validateNativeVideoCard } from './videoTariffPricing'
const props = defineProps<{ entry: PricingFormEntry }>()
const emit = defineEmits<{ update: [entry: PricingFormEntry] }>()
const { t } = useI18n()
const search = ref('')
const page = ref(0)
const pageSize = 20
const labels = computed(() => new Map(nativeVideoCells(props.entry.models[0]).map(cell =>
  [cell.key, Object.entries(cell.controls).map(([key, value]) => `${key}=${value}`).join(' · ')])))
const label = (key: string) => labels.value.get(key) || key
const filtered = computed(() => props.entry.intervals.filter(row => label(row.tier_label).toLowerCase().includes(search.value.toLowerCase())))
const visible = computed(() => filtered.value.slice(page.value * pageSize, (page.value + 1) * pageSize))
const error = computed(() => validateNativeVideoCard(props.entry))
function edit(key: string, price: string) {
  emit('update', { ...props.entry, intervals: props.entry.intervals.map(row =>
    row.tier_label === key ? { ...row, per_request_price: price } : row) })
}
function setFree(key: string, enabled: boolean) {
  const metadata = props.entry.video_tariff!
  const cell = { ...metadata.cells[key] }
  if (enabled) cell.explicit_free = true
  else delete cell.explicit_free
  emit('update', { ...props.entry, video_tariff: {
    ...metadata, cells: { ...metadata.cells, [key]: cell },
  } })
}
</script>
