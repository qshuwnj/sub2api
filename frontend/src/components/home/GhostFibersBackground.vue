<template>
  <div
    class="home-fibers"
    :class="{ 'home-fibers--dark': dark }"
    data-testid="home-fibers"
    :data-renderer="state"
    aria-hidden="true"
  >
    <div ref="container" class="home-fibers__scene">
      <canvas
        ref="canvas"
        class="home-fibers__canvas"
        tabindex="-1"
        @webglcontextlost.prevent="handleContextLost"
        @webglcontextrestored="start"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { createGhostFibers, type GhostFibersOptions, type GhostFibersRenderer } from './ghost-fibers/renderer.js'

const props = withDefaults(defineProps<{ dark?: boolean }>(), { dark: false })
const container = ref<HTMLDivElement | null>(null)
const canvas = ref<HTMLCanvasElement | null>(null)
const state = ref<'static' | 'animated' | 'fallback'>('static')
let renderer: GhostFibersRenderer | null = null
let motion: MediaQueryList | undefined
let smallScreen: MediaQueryList | undefined

function options(): GhostFibersOptions {
  const mobile = smallScreen?.matches === true
  if (props.dark) {
    // Background Studio's Violet preset; keep the same fibers on every screen.
    return {
      glowColor: '#8b5cf6',
      lightMode: false,
      layers: 3,
      threads: 72,
      density: 1,
      fiberStyle: 'dots',
      twist: 1.2,
      depthOfField: 0.85,
      dpr: Math.min(window.devicePixelRatio || 1, mobile ? 1 : 2),
    }
  }
  return {
    glowColor: '#08aebd',
    lineColor: '#1593ae',
    secondaryColor: '#8654cf',
    speed: 0.18,
    rotation: -24,
    rollSpeed: 0.035,
    layers: mobile ? 2 : 3,
    threads: mobile ? 24 : 48,
    density: mobile ? 0.5 : 0.8,
    scale: 1.05,
    waves: 1.05,
    twist: 0.85,
    volume: 0.45,
    spread: 1.3,
    thickness: 0.85,
    flow: 0.12,
    fiberStyle: 'lines',
    depthOfField: 0.3,
    particles: 0,
    sparkle: 0.08,
    glow: 0,
    brightness: 1.35,
    vignette: 0.2,
    grain: 0,
    mouseInteraction: false,
    pulseOnClick: false,
    intro: false,
    lightMode: true,
    dpr: Math.min(window.devicePixelRatio || 1, mobile ? 1 : 1.25),
  }
}

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
    renderer = createGhostFibers(container.value, canvas.value, options())
    if (renderer) state.value = 'animated'
  } catch {
    // The decorative background must never prevent access to the site.
    stop()
  }
}

function handleContextLost() {
  stop()
  state.value = 'fallback'
}

watch(() => props.dark, () => renderer?.update(options()))

onMounted(() => {
  motion = window.matchMedia('(prefers-reduced-motion: reduce)')
  smallScreen = window.matchMedia('(max-width: 767px)')
  motion.addEventListener?.('change', start)
  smallScreen.addEventListener?.('change', start)
  start()
})

onBeforeUnmount(() => {
  motion?.removeEventListener?.('change', start)
  smallScreen?.removeEventListener?.('change', start)
  stop()
})
</script>

<style scoped>
.home-fibers {
  position: fixed;
  inset: 0;
  z-index: 0;
  overflow: hidden;
  pointer-events: none;
  background: #f7fbfc;
}

.home-fibers__canvas {
  display: block;
  width: 100%;
  height: 100%;
  opacity: 0.85;
}

.home-fibers__scene {
  position: absolute;
  inset: 0;
}

.home-fibers--dark {
  background: #120f17;
}

.home-fibers--dark .home-fibers__canvas {
  opacity: 1;
}

.home-fibers:not([data-renderer='animated']) .home-fibers__canvas {
  visibility: hidden;
}

@media (min-width: 768px) {
  /* Preserve the reference's landscape framing instead of a tall, enlarged crop. */
  .home-fibers--dark .home-fibers__scene {
    inset: auto;
    left: 50%;
    top: 50%;
    width: max(100vw, 177.7778vh);
    height: max(100vh, 56.25vw);
    transform: translate(-50%, -50%);
  }
}

@media (max-width: 767px) {
  .home-fibers__canvas {
    opacity: 0.65;
  }
}
</style>
