<template>
  <div
    class="relative flex min-h-screen items-center justify-center overflow-hidden p-4"
    :class="{ 'dark login-rings': showMagicRings }"
  >
    <MagicRingsBackground v-if="showMagicRings" />
    <!-- Background -->
    <div
      v-if="!showMagicRings"
      class="absolute inset-0 bg-gradient-to-br from-gray-50 via-primary-50/30 to-gray-100 dark:from-dark-950 dark:via-dark-900 dark:to-dark-950"
    ></div>

    <!-- Decorative Elements -->
    <div v-if="!showMagicRings" class="pointer-events-none absolute inset-0 overflow-hidden">
      <!-- Gradient Orbs -->
      <div
        class="absolute -right-40 -top-40 h-80 w-80 rounded-full bg-primary-400/20 blur-3xl"
      ></div>
      <div
        class="absolute -bottom-40 -left-40 h-80 w-80 rounded-full bg-primary-500/15 blur-3xl"
      ></div>
      <div
        class="absolute left-1/2 top-1/2 h-96 w-96 -translate-x-1/2 -translate-y-1/2 rounded-full bg-primary-300/10 blur-3xl"
      ></div>

      <!-- Grid Pattern -->
      <div
        class="absolute inset-0 bg-[linear-gradient(rgba(20,184,166,0.03)_1px,transparent_1px),linear-gradient(90deg,rgba(20,184,166,0.03)_1px,transparent_1px)] bg-[size:64px_64px]"
      ></div>
    </div>

    <!-- Content Container -->
    <div class="auth-content relative z-10 w-full max-w-md">
      <!-- Logo/Brand -->
      <div class="mb-8 text-center">
        <!-- Custom Logo or Default Logo -->
        <template v-if="settingsLoaded">
          <HomeElectricLogo
            v-if="showMagicRings"
            class="auth-electric-logo"
            :alt="siteName"
            :dark="true"
          />
          <div
            v-else
            class="mb-4 inline-flex h-16 w-16 items-center justify-center overflow-hidden rounded-2xl shadow-lg shadow-primary-500/30"
          >
            <img :src="siteLogo || '/logo.svg'" alt="Logo" class="h-full w-full object-contain" />
          </div>
          <h1 class="auth-brand-title text-gradient mb-2 text-3xl font-bold">
            {{ siteName }}
          </h1>
          <p class="text-sm text-gray-500 dark:text-dark-400">
            {{ siteSubtitle }}
          </p>
        </template>
      </div>

      <!-- Card Container -->
      <div class="auth-card card-glass rounded-2xl p-8 shadow-glass">
        <slot />
      </div>

      <!-- Footer Links -->
      <div class="mt-6 text-center text-sm">
        <slot name="footer" />
      </div>

      <!-- Copyright -->
      <div class="mt-8 text-center text-xs text-gray-400 dark:text-dark-500">
        &copy; {{ currentYear }} {{ siteName }}. All rights reserved.
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, defineAsyncComponent, onMounted } from 'vue'
import { useAppStore } from '@/stores'
import { sanitizeUrl } from '@/utils/url'

const appStore = useAppStore()
const props = withDefaults(defineProps<{ magicRings?: boolean }>(), { magicRings: false })
const MagicRingsBackground = defineAsyncComponent(() => import('@/components/auth/MagicRingsBackground.vue'))
const HomeElectricLogo = defineAsyncComponent(() => import('@/components/home/HomeElectricLogo.vue'))
const showMagicRings = computed(() => props.magicRings)

const siteName = computed(() => appStore.siteName || 'Sub2API')
const siteLogo = computed(() => sanitizeUrl(appStore.siteLogo || '', { allowRelative: true, allowDataUrl: true }))
const siteSubtitle = computed(() => appStore.cachedPublicSettings?.site_subtitle || 'Subscription to API Conversion Platform')
const settingsLoaded = computed(() => appStore.publicSettingsLoaded)

const currentYear = computed(() => new Date().getFullYear())

onMounted(() => {
  appStore.fetchPublicSettings()
})
</script>

<style scoped>
.text-gradient {
  @apply bg-gradient-to-r from-primary-600 to-primary-500 bg-clip-text text-transparent;
}

.login-rings {
  isolation: isolate;
  background: #100d16;
  color: #f3eefb;
  color-scheme: dark;
}

.login-rings .auth-content { padding-block: 32px; }
.login-rings .auth-electric-logo {
  width: 136px;
  height: 82px;
  margin: 0 auto 16px;
}
.login-rings .auth-electric-logo :deep(.home-electric-logo) {
  left: -27px;
  top: -27px;
  width: 190px;
  height: 136px;
}
.login-rings .auth-electric-logo :deep(.home-electric-logo__fallback) {
  width: 164px;
  height: 164px;
}
.login-rings .auth-brand-title { background: none; color: #f3eefb; }
.login-rings .auth-card {
  border-color: rgb(197 172 226 / 20%);
  background: rgb(23 19 31 / 96%);
  box-shadow: 0 20px 56px -24px #000c;
  backdrop-filter: none;
}

.login-rings :deep(.input) {
  border-color: #51445f;
  background: #211a2b;
  color: #f3eefb;
  caret-color: #d9b8ff;
}
.login-rings :deep(.input::placeholder) { color: #b2a6c2; }
.login-rings :deep(.input:focus) { border-color: #c89df7; --tw-ring-color: rgb(200 157 247 / 30%); }
.login-rings :deep(.input-error) { border-color: #fb7185; }
.login-rings :deep(.input-label) { color: #ddd1ed; }
.login-rings :deep(a) { color: #d1b1f4; }
.login-rings :deep(a:hover) { color: #ead9ff; }
.login-rings :deep(.btn-primary) {
  border-color: #d9b8ff;
  background: #bf94fa;
  color: #241031;
  box-shadow: 0 6px 20px -10px rgb(175 117 247 / 45%);
}
.login-rings :deep(.btn-primary:hover:not(:disabled)) { background: #d3afff; }
.login-rings :deep(:focus-visible) { outline: 2px solid #d9b8ff; outline-offset: 3px; }
.login-rings :deep(::selection) { background: #674887; color: #fff; }

@media (max-width: 479px) {
  .login-rings .auth-card { padding: 24px; }
  .login-rings .auth-content { padding-block: 20px; }
}
</style>
