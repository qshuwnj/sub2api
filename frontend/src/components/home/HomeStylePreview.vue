<template>
  <div class="style-preview" data-testid="home-style-preview">
    <GhostFibersBackground :dark="true" />

    <header class="preview-header">
      <a class="brand" href="/home?preview=styles" aria-label="QuotaFlow API 首页">
        <img v-if="siteLogo" :src="siteLogo" alt="" width="38" height="38" />
        <span>{{ siteName }}</span>
      </a>
      <nav aria-label="网站导航">
        <a v-if="docUrl" :href="docUrl" target="_blank" rel="noopener noreferrer" class="docs-link">接入文档</a>
        <RouterLink :to="entryPath" class="nav-login">
          {{ isAuthenticated ? '控制台' : '登录' }}
          <Icon name="arrowRight" size="sm" aria-hidden="true" />
        </RouterLink>
      </nav>
    </header>

    <main :data-style="selected" :class="`layout-${selected}`">
      <template v-if="selected === 'a'">
        <section class="immersive-hero" aria-labelledby="immersive-title">
          <h1 id="immersive-title">{{ siteName }}</h1>
          <h2>连接模型，<br />让想法自由生长。</h2>
          <p class="hero-description">从一次 API 调用，到一张图、一段视频。<br class="desktop-break" />把不同模型的能力，接入你的下一次创作。</p>
          <div class="hero-actions">
            <RouterLink :to="entryPath" class="action action-primary">
              {{ isAuthenticated ? '进入控制台' : '立即开始' }}
              <Icon name="arrowRight" size="sm" aria-hidden="true" />
            </RouterLink>
            <a href="#capabilities-a" class="action action-outline">
              探索更多
              <Icon name="arrowDown" size="sm" aria-hidden="true" />
            </a>
          </div>
        </section>
        <section id="capabilities-a" class="capability-strip" aria-label="平台能力">
          <div><Icon name="key" size="md" aria-hidden="true" /><span><strong>一个密钥，多种模型</strong><small>统一接入已支持的 AI 模型</small></span></div>
          <div><Icon name="sparkles" size="md" aria-hidden="true" /><span><strong>不止对话，也能创作</strong><small>在创意工坊生成图片与视频</small></span></div>
          <div><Icon name="chart" size="md" aria-hidden="true" /><span><strong>按量使用，清晰掌握</strong><small>在控制台查看用量与消费</small></span></div>
        </section>
      </template>

      <template v-else-if="selected === 'b'">
        <section class="minimal-hero" aria-labelledby="minimal-title">
          <h1 id="minimal-title">{{ siteName }}</h1>
          <h2>你的想法，不必止于一种模型。</h2>
          <p class="hero-description">统一的 API 入口。自由的创作空间。</p>
          <div class="hero-actions">
            <RouterLink :to="entryPath" class="action action-primary">
              {{ isAuthenticated ? '打开控制台' : '开始使用' }}
              <Icon name="arrowRight" size="sm" aria-hidden="true" />
            </RouterLink>
            <a v-if="docUrl" :href="docUrl" target="_blank" rel="noopener noreferrer" class="text-action">
              阅读接入文档 <Icon name="externalLink" size="sm" aria-hidden="true" />
            </a>
          </div>
          <div class="model-names" aria-label="已支持的模型平台">
            <span>Claude</span><span>GPT</span><span>Gemini</span><span>Antigravity</span>
          </div>
        </section>
        <section class="minimal-bottom" aria-label="产品入口">
          <span>从开发，到创作。</span>
          <RouterLink :to="entryPath">API 接入 <Icon name="arrowRight" size="sm" aria-hidden="true" /></RouterLink>
          <RouterLink v-if="workshopPath" :to="workshopEntryPath">创意工坊 <Icon name="arrowRight" size="sm" aria-hidden="true" /></RouterLink>
        </section>
      </template>

      <template v-else>
        <section class="product-intro" aria-labelledby="product-title">
          <h1 id="product-title">{{ siteName }}</h1>
          <h2>开发与创作，<br />在这里交汇。</h2>
          <p class="hero-description">为应用接入 AI，或让灵感成为作品。<br />选择你的开始。</p>
        </section>
        <section class="product-paths" aria-label="选择使用方式">
          <div class="developer-path">
            <Icon name="terminal" size="lg" aria-hidden="true" />
            <h3>为应用接入模型</h3>
            <p>获取 API 密钥，将已支持的模型接入你的产品与工作流。</p>
            <RouterLink :to="entryPath" class="path-link">API 接入 <Icon name="arrowRight" size="md" aria-hidden="true" /></RouterLink>
          </div>
          <div v-if="workshopPath" class="creator-path">
            <Icon name="sparkles" size="lg" aria-hidden="true" />
            <h3>让灵感成为作品</h3>
            <p>打开创意工坊，选择模型，用文字开始图片与视频创作。</p>
            <RouterLink :to="workshopEntryPath" class="path-link">进入创意工坊 <Icon name="arrowRight" size="md" aria-hidden="true" /></RouterLink>
          </div>
        </section>
      </template>
    </main>

    <footer class="preview-footer">
      <span>© {{ new Date().getFullYear() }} {{ siteName }}</span>
      <span>Subscription to API Conversion Platform</span>
    </footer>

    <aside class="variant-toolbar" aria-label="本地样式预览">
      <span class="preview-label">本地预览</span>
      <div role="group" aria-label="选择首页样式">
        <button
          v-for="variant in variants"
          :key="variant.id"
          type="button"
          :data-style="variant.id"
          :aria-pressed="selected === variant.id"
          @click="selectVariant(variant.id)"
        ><span class="variant-letter">{{ variant.id.toUpperCase() }}</span>{{ variant.label }}</button>
      </div>
    </aside>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import GhostFibersBackground from './GhostFibersBackground.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{
  siteName: string
  siteLogo: string
  docUrl: string
  isAuthenticated: boolean
  dashboardPath: string
  workshopPath: string
}>()

type Variant = 'a' | 'b' | 'c'
const variants: { id: Variant; label: string }[] = [
  { id: 'a', label: '沉浸品牌' },
  { id: 'b', label: '居中极简' },
  { id: 'c', label: '产品入口' },
]
function readVariant(): Variant {
  const value = new URLSearchParams(window.location.search).get('style')
  return value === 'b' || value === 'c' ? value : 'a'
}
const selected = ref<Variant>(readVariant())
const entryPath = computed(() => props.isAuthenticated ? props.dashboardPath : '/login')
const workshopEntryPath = computed(() => props.isAuthenticated
  ? props.workshopPath
  : `/login?redirect=${encodeURIComponent(props.workshopPath)}`)

function selectVariant(variant: Variant) {
  selected.value = variant
  const url = new URL(window.location.href)
  url.searchParams.set('style', variant)
  window.history.replaceState(window.history.state, '', url)
}
function syncVariant() {
  selected.value = readVariant()
}
onMounted(() => window.addEventListener('popstate', syncVariant))
onBeforeUnmount(() => window.removeEventListener('popstate', syncVariant))
</script>

<style scoped>
.style-preview {
  --ink: #faf9fc;
  --muted: #c4bfce;
  --accent: #99efd9;
  --rule: rgb(235 230 245 / 17%);
  position: relative;
  isolation: isolate;
  min-height: 100svh;
  color: var(--ink);
  background: #120f17;
  color-scheme: dark;
  letter-spacing: 0;
}
.style-preview ::selection { color: #120f17; background: #99efd9; }
.style-preview :is(a, button):focus-visible { outline: 2px solid var(--accent); outline-offset: 6px; }
.style-preview a { text-decoration: none; }
.preview-header, main, .preview-footer { position: relative; z-index: 1; }
.preview-header {
  max-width: 1280px;
  margin: auto;
  padding: 28px 48px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
}
.brand { display: inline-flex; align-items: center; gap: 12px; color: var(--ink); font-size: 18px; font-weight: 700; }
.brand img { border-radius: 8px; flex: none; object-fit: contain; }
.preview-header nav { display: flex; align-items: center; gap: 32px; font-size: 14px; }
.docs-link { color: var(--muted); padding: 12px 0; }
.docs-link:hover, .text-action:hover { color: white; text-decoration: underline; text-underline-offset: 5px; }
.nav-login { color: var(--ink); display: inline-flex; gap: 12px; align-items: center; min-height: 44px; }
main { max-width: 1280px; min-height: calc(100svh - 190px); margin: auto; padding: 0 48px; }
h1, h2, h3, p { margin: 0; }
h1 { font-size: 64px; font-weight: 750; line-height: 1.13; overflow-wrap: anywhere; }
h2 { font-size: 56px; line-height: 1.24; font-weight: 600; text-wrap: balance; }
.hero-description { color: var(--muted); font-size: 17px; line-height: 1.9; max-width: 64ch; }
.hero-actions { display: flex; align-items: center; flex-wrap: wrap; gap: 16px; margin-top: 36px; }
.action { display: inline-flex; min-height: 50px; padding: 0 25px; align-items: center; justify-content: center; gap: 22px; border-radius: 6px; font-size: 15px; font-weight: 600; transition: background .18s, color .18s; }
.action-primary { color: #14251f; background: var(--accent); }
.action-primary:hover { background: #c4f9ec; }
.action-outline { border: 1px solid rgb(244 239 255 / 32%); color: var(--ink); }
.action-outline:hover { background: rgb(244 239 255 / 9%); }
.immersive-hero { padding: 94px 0 100px; min-height: 610px; max-width: 800px; }
.immersive-hero h1 { margin-bottom: 23px; }
.immersive-hero h2 { margin-bottom: 23px; }
.capability-strip { display: flex; gap: 34px; padding: 32px 0; border-top: 1px solid var(--rule); scroll-margin-top: 24px; }
.capability-strip > div { display: flex; align-items: flex-start; gap: 16px; flex: 1; }
.capability-strip > div > svg { color: var(--accent); flex: none; margin-top: 3px; }
.capability-strip strong { display: block; font-size: 15px; font-weight: 550; }
.capability-strip small { display: block; color: var(--muted); font-size: 13px; line-height: 1.7; margin-top: 7px; }
.layout-b { display: flex; flex-direction: column; }
.minimal-hero { text-align: center; padding: 126px 0 120px; flex: 1; }
.minimal-hero h1 { font-size: 84px; }
.minimal-hero h2 { font-size: 34px; margin: 30px 0 16px; font-weight: 500; }
.minimal-hero .hero-description { margin: auto; }
.minimal-hero .hero-actions { justify-content: center; margin-top: 34px; }
.minimal-hero .action-primary { background: #f7f5fb; color: #1a1425; }
.minimal-hero .action-primary:hover { background: #dcd6e8; }
.text-action { display: inline-flex; min-height: 48px; padding: 0 16px; align-items: center; gap: 10px; font-size: 14px; color: var(--muted); }
.model-names { display: flex; justify-content: center; flex-wrap: wrap; gap: 32px; margin-top: 64px; font-size: 15px; color: #d8d2e5; }
.minimal-bottom { display: flex; gap: 32px; align-items: center; padding: 26px 0; border-top: 1px solid var(--rule); font-size: 14px; }
.minimal-bottom > span { color: var(--muted); margin-right: auto; }
.minimal-bottom a { display: inline-flex; gap: 15px; align-items: center; color: var(--ink); min-height: 44px; }
.minimal-bottom a:hover { color: var(--accent); }
.layout-c { padding-top: 63px; }
.product-intro h1 { font-size: 44px; margin-bottom: 24px; }
.product-intro h2 { font-size: 58px; margin-bottom: 22px; }
.product-paths { display: grid; grid-template-columns: 1fr 1fr; gap: 80px; margin-top: 60px; padding-bottom: 35px; }
.product-paths > div { padding: 25px 0 0; border-top: 1px solid var(--rule); max-width: 440px; }
.product-paths .creator-path { margin-top: -90px; justify-self: end; }
.developer-path > svg { color: var(--accent); }
.creator-path > svg { color: #f9c093; }
.product-paths h3 { font-size: 24px; font-weight: 550; margin: 20px 0 12px; }
.product-paths p { color: var(--muted); font-size: 15px; line-height: 1.8; max-width: 37ch; }
.path-link { display: flex; align-items: center; justify-content: space-between; min-height: 52px; color: var(--accent); font-size: 15px; margin-top: 22px; border-bottom: 1px solid var(--rule); }
.creator-path .path-link { color: #f9c093; }
.path-link:hover { color: white; }
.preview-footer { max-width: 1280px; margin: auto; padding: 24px 48px 112px; color: #b7afc3; font-size: 12px; display: flex; justify-content: space-between; gap: 24px; }
.variant-toolbar { position: fixed; z-index: 10; bottom: 22px; left: 50%; transform: translateX(-50%); background: #211c2a; border: 1px solid #494050; border-radius: 8px; padding: 6px 8px 6px 16px; display: flex; align-items: center; gap: 16px; box-shadow: 0 8px 32px rgb(0 0 0 / 35%); }
.preview-label { color: #bdb5cc; font-size: 12px; white-space: nowrap; }
.variant-toolbar [role="group"] { display: flex; gap: 4px; }
.variant-toolbar button { display: inline-flex; justify-content: center; align-items: center; gap: 8px; min-height: 44px; padding: 0 14px; border: 0; border-radius: 4px; color: #d4ccdf; background: transparent; font-size: 12px; white-space: nowrap; cursor: pointer; }
.variant-toolbar button:hover { background: #342c3f; }
.variant-toolbar button[aria-pressed="true"] { background: #f3eff9; color: #241c31; }
.variant-letter { font-weight: 750; font-size: 13px; }
@media (max-width: 1000px) {
  .preview-header, main, .preview-footer { padding-left: 32px; padding-right: 32px; }
  h1 { font-size: 56px; }
  h2 { font-size: 48px; }
  .minimal-hero h1 { font-size: 68px; }
  .product-paths { gap: 48px; }
}
@media (max-width: 767px) {
  .preview-header { padding: 20px 24px; gap: 12px; }
  .brand { font-size: 15px; gap: 9px; }
  .brand img { width: 32px; height: 32px; }
  .preview-header nav { gap: 12px; }
  .docs-link { display: none; }
  main { padding: 0 24px; min-height: calc(100svh - 190px); }
  h1 { font-size: 39px; }
  h2 { font-size: 38px; }
  .hero-description { font-size: 15px; line-height: 1.85; }
  .immersive-hero { padding: 65px 0 72px; min-height: 0; }
  .immersive-hero h1 { margin-bottom: 24px; }
  .immersive-hero h2 { margin-bottom: 22px; }
  .hero-actions { gap: 12px; margin-top: 28px; }
  .action { padding: 0 18px; gap: 15px; font-size: 14px; }
  .capability-strip { flex-direction: column; gap: 25px; padding: 28px 0; }
  .capability-strip strong { font-size: 14px; }
  .minimal-hero { padding: 94px 0 86px; }
  .minimal-hero h1 { font-size: 44px; }
  .minimal-hero h2 { font-size: 27px; line-height: 1.5; margin-top: 26px; }
  .minimal-hero .hero-actions { flex-direction: column; gap: 6px; }
  .model-names { gap: 18px; margin-top: 42px; font-size: 13px; }
  .minimal-bottom { flex-wrap: wrap; gap: 12px 28px; padding: 24px 0; }
  .minimal-bottom > span { width: 100%; }
  .layout-c { padding-top: 48px; }
  .product-intro h1 { font-size: 33px; margin-bottom: 22px; }
  .product-intro h2 { font-size: 42px; }
  .product-paths { grid-template-columns: 1fr; gap: 34px; margin-top: 46px; }
  .product-paths > div { max-width: none; width: 100%; }
  .product-paths .creator-path { margin-top: 0; }
  .product-paths h3 { font-size: 22px; margin-top: 16px; }
  .preview-footer { padding: 28px 24px 106px; flex-direction: column; gap: 8px; line-height: 1.6; }
  .variant-toolbar { bottom: 12px; width: calc(100% - 24px); max-width: 480px; padding: 5px; gap: 0; }
  .preview-label { display: none; }
  .variant-toolbar [role="group"] { width: 100%; }
  .variant-toolbar button { flex: 1; padding: 0 7px; gap: 5px; }
}
@media (prefers-reduced-motion: reduce) {
  .action { transition: none; }
}
</style>
