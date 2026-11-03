<template>
  <div
    ref="container"
    class="login-magic-rings"
    data-testid="login-magic-rings"
    :data-renderer="state"
    aria-hidden="true"
  >
    <div v-if="state !== 'animated'" class="login-magic-rings__static">
      <span v-for="ring in 4" :key="ring" :style="{ '--ring': ring }" />
    </div>
    <canvas
      ref="canvas"
      tabindex="-1"
      @webglcontextlost.prevent="handleContextLost"
      @webglcontextrestored="start"
    />
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { createMagicRings, type MagicRingsRenderer } from './magic-rings/renderer.js'

const container = ref<HTMLElement | null>(null)
const canvas = ref<HTMLCanvasElement | null>(null)
const state = ref<'static' | 'animated' | 'fallback'>('static')
let renderer: MagicRingsRenderer | null | undefined
let motion: MediaQueryList | undefined
let mobile: MediaQueryList | undefined

function stop() {
  renderer?.dispose()
  renderer = null
}

function start() {
  stop()
  if (motion?.matches) {
    state.value = 'static'
    return
  }
  state.value = 'fallback'
  if (!container.value || !canvas.value || typeof WebGL2RenderingContext === 'undefined') return
  try {
    renderer = createMagicRings(container.value, canvas.value, {
      color: '#A855F7',
      colorTwo: '#6366F1',
      intro: false,
      maxDevicePixelRatio: mobile?.matches ? 1 : 1.5,
    })
    if (renderer) state.value = 'animated'
  } catch {
    stop()
  }
}

function handleContextLost() {
  stop()
  state.value = 'fallback'
}

onMounted(() => {
  motion = window.matchMedia('(prefers-reduced-motion: reduce)')
  mobile = window.matchMedia('(max-width: 767px)')
  motion.addEventListener?.('change', start)
  mobile.addEventListener?.('change', start)
  start()
})

onBeforeUnmount(() => {
  motion?.removeEventListener?.('change', start)
  mobile?.removeEventListener?.('change', start)
  stop()
})
</script>

<style scoped>
.login-magic-rings {
  position: fixed;
  inset: 0;
  overflow: hidden;
  pointer-events: none;
  background: #100d16;
}

.login-magic-rings canvas {
  display: block;
  width: 100%;
  height: 100%;
}

.login-magic-rings:not([data-renderer='animated']) canvas { visibility: hidden; }

.login-magic-rings__static span {
  position: absolute;
  left: 50%;
  top: 50%;
  width: calc(240px + var(--ring) * 120px);
  aspect-ratio: 1;
  border: 1px solid rgb(171 108 240 / 22%);
  border-radius: 50%;
  transform: translate(-50%, -50%);
}
</style>
