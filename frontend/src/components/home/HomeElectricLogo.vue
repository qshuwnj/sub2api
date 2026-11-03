<template>
  <div class="home-electric-brand">
    <div
      ref="container"
      class="home-electric-logo"
      data-testid="electric-logo"
      :data-renderer="state"
      role="img"
      :aria-label="alt"
    >
      <img :src="src" alt="" class="home-electric-logo__fallback" aria-hidden="true" />
      <canvas
        ref="canvas"
        class="home-electric-logo__canvas"
        aria-hidden="true"
        tabindex="-1"
        @webglcontextlost.prevent="handleContextLost"
        @webglcontextrestored="start"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { createElectricLogo, type ElectricLogoRenderer } from './electric-logo/renderer.js'
import cleanLogo from './electric-logo/quotaflow-clean.png'

const props = withDefaults(defineProps<{ src?: string; alt?: string; dark?: boolean }>(), {
  src: cleanLogo,
  alt: 'QuotaFlow API',
  dark: true,
})
const container = ref<HTMLElement | null>(null)
const canvas = ref<HTMLCanvasElement | null>(null)
const state = ref<'static' | 'loading' | 'animated' | 'fallback'>('static')
let renderer: ElectricLogoRenderer | null = null
let motion: MediaQueryList | undefined
let generation = 0

function stop() {
  generation++
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
  const current = generation
  state.value = 'loading'
  try {
    renderer = createElectricLogo(container.value, canvas.value, {
      src: props.src,
      color: '#ecc7ff',
      glowColor: '#ad6dff',
      scale: 0.7,
      strands: 4,
      bend: 0.6,
      crackle: 1.5,
      arcs: 1,
      speed: 2.5,
      interactive: true,
      dark: props.dark,
      onReady: () => { if (current === generation) state.value = 'animated' },
      onError: () => { if (current === generation) state.value = 'fallback' },
    })
    if (!renderer) state.value = 'fallback'
  } catch {
    stop()
    state.value = 'fallback'
  }
}

function handleContextLost() {
  stop()
  state.value = 'fallback'
}

watch(() => [props.src, props.dark], start, { flush: 'post' })

onMounted(() => {
  motion = window.matchMedia('(prefers-reduced-motion: reduce)')
  motion.addEventListener?.('change', start)
  start()
})

onBeforeUnmount(() => {
  motion?.removeEventListener?.('change', start)
  stop()
})
</script>

<style scoped>
.home-electric-brand {
  position: relative;
  flex-shrink: 0;
  width: 80px;
  height: 48px;
}

.home-electric-logo {
  position: absolute;
  left: -16px;
  top: -16px;
  width: 112px;
  height: 80px;
  overflow: visible;
}

.home-electric-logo__canvas {
  display: block;
  width: 100%;
  height: 100%;
  /* Let the halo disappear before it reaches the drawing surface's edges. */
  mask-image:
    linear-gradient(to bottom, transparent, #000 15%, #000 85%, transparent),
    linear-gradient(to right, transparent, #000 8%, #000 92%, transparent);
  mask-composite: intersect;
}

.home-electric-logo__fallback {
  position: absolute;
  left: 50%;
  top: 50%;
  width: 96px;
  height: 96px;
  object-fit: contain;
  transform: translate(-50%, -50%);
}

.home-electric-logo:not([data-renderer='animated']) .home-electric-logo__canvas { visibility: hidden; }
.home-electric-logo[data-renderer='animated'] .home-electric-logo__fallback { visibility: hidden; }

</style>
