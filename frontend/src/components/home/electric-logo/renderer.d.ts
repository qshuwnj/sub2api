export interface ElectricLogoOptions {
  src: string
  color: string
  glowColor: string
  scale: number
  strands: number
  bend: number
  crackle: number
  arcs: number
  speed: number
  interactive: boolean
  dark: boolean
  onReady?: () => void
  onError?: () => void
}

export interface ElectricLogoRenderer {
  dispose(): void
}

export function createElectricLogo(
  container: HTMLElement,
  canvas: HTMLCanvasElement,
  options: ElectricLogoOptions,
): ElectricLogoRenderer | null
