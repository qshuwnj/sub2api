<template>
  <fieldset class="mt-3">
    <legend class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.channels.form.imageQualityPrices') }}</legend>
    <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(nativeProfile ? 'admin.channels.form.imageNativePricesHint' : 'admin.channels.form.imageQualityPricesHint') }}</p>
    <div v-if="preset.length" class="mt-3">
      <button type="button" data-testid="image-price-preset" class="btn btn-secondary text-xs"
        @click="emit('update', preset)">{{ t(nativeProfile ? 'admin.channels.form.imageNativePreset' : 'admin.channels.form.imageQualityPreset') }}</button>
      <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(nativeProfile ? 'admin.channels.form.imageNativePresetHint' : 'admin.channels.form.imageQualityPresetHint') }}</p>
    </div>
    <p v-else-if="nativeProfile" role="status" class="mt-2 text-xs text-amber-700 dark:text-amber-300">{{ t('admin.channels.form.imageNativePresetPending') }}</p>
    <div v-for="size in profile.sizes" :key="size.value" class="mt-3">
      <p class="text-xs font-medium text-gray-600 dark:text-gray-300">
        {{ size.ratio }} <span v-if="size.value !== 'auto'" class="ml-2 font-normal tabular-nums">{{ size.label }}{{ 'native' in size ? '' : ' px' }}</span>
      </p>
      <div class="mt-1 grid grid-cols-3 gap-2">
        <label v-for="quality in profile.qualities" :key="quality" class="min-w-0 text-xs text-gray-600 dark:text-gray-300">
          {{ t(`admin.channels.form.imageQuality_${quality}`) }} <span>USD</span>
          <input :value="imageQualityPriceText(price(quality, size.value))" type="text" inputmode="decimal"
            :aria-label="`${size.ratio} ${size.label} ${t(`admin.channels.form.imageQuality_${quality}`)} USD`"
            :aria-invalid="price(quality, size.value) !== '' && !isValidImageQualityPrice(price(quality, size.value))"
            @input="emit('update', setImageQualitySizePrice(intervals, quality, size.value, ($event.target as HTMLInputElement).value, models))"
            class="input mt-1 min-w-0 text-sm tabular-nums" :placeholder="t('admin.channels.form.imageQualityUnset')" />
        </label>
      </div>
    </div>
    <p v-if="invalid" role="alert" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ t('admin.channels.form.imageQualityInvalid') }}</p>
  </fieldset>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { IntervalFormEntry } from './types'
import { imageQualityProfile, verifiedImagePricePreset, imageQualityPriceText, imageQualitySizePrice, isValidImageQualityPrice, setImageQualitySizePrice, type ImageQuality, type ImageSize } from './imageQualityPricing'

const props = defineProps<{ intervals: IntervalFormEntry[]; models?: string[] }>()
const emit = defineEmits<{ update: [intervals: IntervalFormEntry[]] }>()
const { t } = useI18n()
const profile = computed(() => imageQualityProfile(props.models))
const nativeProfile = computed(() => profile.value.sizes.some(size => 'native' in size))
const preset = computed(() => verifiedImagePricePreset(props.models || ['openai/gpt-image-1-mini/text-to-image']))
const price = (quality: ImageQuality, size: ImageSize) => imageQualitySizePrice(props.intervals, quality, size)
const invalid = computed(() => props.intervals.some(row => !isValidImageQualityPrice(row.per_request_price)))
</script>
