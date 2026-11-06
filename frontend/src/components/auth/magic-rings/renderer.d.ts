export interface MagicRingsRenderer {
  dispose(): void
}

export function createMagicRings(
  container: HTMLElement,
  canvas: HTMLCanvasElement,
  options?: {
    color?: string
    colorTwo?: string
    intro?: boolean
    maxDevicePixelRatio?: number
  },
): MagicRingsRenderer | null | undefined
