export interface GhostFibersOptions {
  glowColor?: string
  lineColor?: string
  secondaryColor?: string
  gradient?: 'depth' | 'along' | 'across'
  speed?: number
  scale?: number
  rotation?: number
  rollSpeed?: number
  layers?: number
  waves?: number
  twist?: number
  threads?: number
  volume?: number
  spread?: number
  density?: number
  thickness?: number
  flow?: number
  fiberStyle?: 'dots' | 'dashes' | 'lines'
  depthOfField?: number
  particles?: number
  sparkle?: number
  glow?: number
  brightness?: number
  vignette?: number
  grain?: number
  mouseInteraction?: boolean
  pulseOnClick?: boolean
  intro?: boolean
  lightMode?: boolean
  paused?: boolean
  dpr?: number
}

export interface GhostFibersRenderer {
  dispose(): void
  update(options: GhostFibersOptions): void
}

export function createGhostFibers(
  container: HTMLElement,
  canvas: HTMLCanvasElement,
  options?: GhostFibersOptions,
): GhostFibersRenderer | null
