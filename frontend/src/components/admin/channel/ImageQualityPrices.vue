<template>
  <fieldset class="mt-3">
    <legend class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.channels.form.imageQualityPrices') }}</legend>
    <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.channels.form.imageQualityPricesHint') }}</p>
    <div class="mt-3 grid gap-3 sm:grid-cols-3">
      <label v-for="quality in IMAGE_QUALITIES" :key="quality" class="text-sm text-gray-600 dark:text-gray-300">
        {{ t(`admin.channels.form.imageQuality_${quality}`) }} <span class="text-xs">USD</span>
        <input :value="imageQualityPriceText(price(quality))" type="text" inputmode="decimal"
          :aria-label="t(`admin.channels.form.imageQuality_${quality}`)"
          :aria-invalid="price(quality) !== '' && !isValidImageQualityPrice(price(quality))"
          @input="emit('update', setImageQualityPrice(intervals, quality, ($event.target as HTMLInputElement).value))"
          class="input mt-1 text-sm" :placeholder="t('admin.channels.form.imageQualityUnset')" />
      </label>
    </div>
    <p v-if="invalid" role="alert" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ t('admin.channels.form.imageQualityInvalid') }}</p>
  </fieldset>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { IntervalFormEntry } from './types'
import { IMAGE_QUALITIES, imageQualityPriceText, isValidImageQualityPrice, setImageQualityPrice, type ImageQuality } from './imageQualityPricing'

const props = defineProps<{ intervals: IntervalFormEntry[] }>()
const emit = defineEmits<{ update: [intervals: IntervalFormEntry[]] }>()
const { t } = useI18n()
const price = (quality: ImageQuality) => props.intervals.find(row => row.tier_label === `quality:${quality}`)?.per_request_price ?? ''
const invalid = computed(() => props.intervals.some(row => !isValidImageQualityPrice(row.per_request_price)))
</script>
