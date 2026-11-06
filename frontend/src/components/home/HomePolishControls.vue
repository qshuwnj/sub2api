<template>
  <div class="home-polish-controls" data-testid="home-polish-controls" role="group" aria-label="首页样式对比">
    <button
      v-for="finish in finishes"
      :key="finish.value"
      type="button"
      :data-finish="finish.value"
      :aria-pressed="modelValue === (finish.value === 'refined')"
      @click="selectFinish(finish.value)"
    >
      {{ finish.label }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted } from 'vue'

defineProps<{ modelValue: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>()
const finishes = [
  { value: 'original', label: '原版' },
  { value: 'refined', label: '精修版' },
] as const

function selectFinish(finish: 'original' | 'refined') {
  const url = new URL(window.location.href)
  url.searchParams.set('finish', finish)
  window.history.replaceState(window.history.state, '', url)
  emit('update:modelValue', finish === 'refined')
}

function syncHistory() {
  emit('update:modelValue', new URLSearchParams(window.location.search).get('finish') !== 'original')
}

onMounted(() => window.addEventListener('popstate', syncHistory))
onUnmounted(() => window.removeEventListener('popstate', syncHistory))
</script>

<style>
/* Loaded only by the loopback DEV preview. Never restyle the shared fiber layer. */
.home-polish-controls {
  position: fixed;
  right: max(16px, env(safe-area-inset-right));
  bottom: max(16px, env(safe-area-inset-bottom));
  z-index: 50;
  display: flex;
  gap: 3px;
  padding: 4px;
  border: 1px solid #51465f;
  border-radius: 8px;
  background: #17131e;
  box-shadow: 0 4px 16px #0005;
}

.home-polish-controls button {
  min-width: 68px;
  min-height: 40px;
  padding: 8px 14px;
  border-radius: 5px;
  color: #c4bbd2;
  font-size: 13px;
  font-weight: 500;
  transition: background-color 150ms, color 150ms;
}

.home-polish-controls button:hover { background: #2c2438; color: #fff; }
.home-polish-controls button[aria-pressed="true"] { background: #eae4f2; color: #231b30; }
.home-polish-controls button:focus-visible { outline: 2px solid #66e1cc; outline-offset: 3px; }

</style>
