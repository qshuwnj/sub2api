import { ARCS, PULSES, PIXEL_BUDGET, hexToRgb, traceShape, spawnArc, vertex, fragment } from './shape-and-shaders.js'

// Native WebGL2 binding for the supplied shaders; no React/OGL runtime.
export function createElectricLogo(container, canvas, options) {
  const gl = canvas.getContext('webgl2', {
    alpha: true, premultipliedAlpha: true, antialias: false, depth: false, powerPreference: 'low-power',
  })
  if (!gl) return null

  let alive = true
  let raf = 0
  let visible = true
  let width = 1
  let height = 1
  let sceneScale = 1
  let shape = null
  let time = 0
  let progress = 0
  let ready = false
  let last = performance.now()
  let burst = null
  const sparks = []
  const pulses = []
  const pointer = { x: 0, y: 0, over: false }
  const hover = { x: 0, y: 0, vx: 0, vy: 0, power: 0 }
  const shaders = []
  const textures = []
  const buffers = []
  const program = gl.createProgram()
  const vao = gl.createVertexArray()
  const uniforms = new Map()
  let resizeObserver
  let intersectionObserver
  let image

  function dispose() {
    if (!alive) return
    alive = false
    cancelAnimationFrame(raf)
    resizeObserver?.disconnect()
    intersectionObserver?.disconnect()
    document.removeEventListener('visibilitychange', onVisibility)
    container.removeEventListener('pointermove', onMove)
    container.removeEventListener('pointerdown', onDown)
    container.removeEventListener('pointerleave', onLeave)
    container.removeEventListener('pointercancel', onLeave)
    if (image) {
      image.onload = null
      image.onerror = null
    }
    textures.forEach(texture => gl.deleteTexture(texture))
    buffers.forEach(buffer => gl.deleteBuffer(buffer))
    shaders.forEach(shader => gl.deleteShader(shader))
    gl.deleteVertexArray(vao)
    gl.deleteProgram(program)
  }

  const fail = () => {
    if (!alive) return
    dispose()
    options.onError?.()
  }

  function location(name) {
    if (!uniforms.has(name)) uniforms.set(name, gl.getUniformLocation(program, name))
    return uniforms.get(name)
  }

  function start() {
    if (!alive || raf || !visible || document.hidden || !shape) return
    last = performance.now()
    raf = requestAnimationFrame(frame)
  }

  function onVisibility() {
    if (document.hidden) {
      cancelAnimationFrame(raf)
      raf = 0
    } else start()
  }

  function onMove(event) {
    const rect = container.getBoundingClientRect()
    pointer.x = (event.clientX - rect.left) * sceneScale
    pointer.y = (event.clientY - rect.top) * sceneScale
    pointer.over = true
  }

  function onDown(event) {
    if (!options.interactive) return
    onMove(event)
    pulses.push({ x: pointer.x, y: pointer.y, born: performance.now() })
    if (pulses.length > PULSES) pulses.shift()
    burst = { x: pointer.x, y: pointer.y }
  }

  function onLeave() { pointer.over = false }

  function resize() {
    const cssWidth = Math.max(1, container.clientWidth)
    const cssHeight = Math.max(1, container.clientHeight)
    // Preserve the 480px demo's stroke proportions at header size without rendering extra pixels.
    sceneScale = 480 / cssHeight
    width = cssWidth * sceneScale
    height = 480
    const dpr = Math.min(window.devicePixelRatio || 1, 2, Math.sqrt(PIXEL_BUDGET / (cssWidth * cssHeight)))
    canvas.width = Math.round(cssWidth * dpr)
    canvas.height = Math.round(cssHeight * dpr)
    gl.viewport(0, 0, canvas.width, canvas.height)
  }

  function frame(now) {
    raf = 0
    if (!alive || !visible || document.hidden || !shape || gl.isContextLost()) return
    const dt = Math.min(0.05, Math.max(0, (now - last) / 1000))
    last = now
    time += dt * options.speed
    progress = Math.min(1, progress + dt / 1.4)
    const presence = progress * progress * (3 - 2 * progress)
    const fit = Math.max(1e-4, Math.min(width * options.scale / shape.logoWidth, height * options.scale / shape.logoHeight))
    const ox = width / 2 - (shape.pad + shape.logoWidth / 2) * fit
    const oy = height / 2 - (shape.pad + shape.logoHeight / 2) * fit
    const unit = Math.max(shape.logoWidth, shape.logoHeight) * fit / 100
    const engaged = options.interactive && pointer.over
    if (engaged && hover.power < 0.01) {
      hover.x = pointer.x
      hover.y = pointer.y
      hover.vx = 0
      hover.vy = 0
    }
    hover.vx += ((pointer.x - hover.x) * 120 - hover.vx * 19) * dt
    hover.vy += ((pointer.y - hover.y) * 120 - hover.vy * 19) * dt
    hover.x += hover.vx * dt
    hover.y += hover.vy * dt
    hover.power += ((engaged ? 1 : 0) - hover.power) * (1 - Math.exp(-dt / (engaged ? 0.3 : 0.55)))

    for (let i = sparks.length - 1; i >= 0; i--) {
      if (time - sparks[i].born > sparks[i].life) sparks.splice(i, 1)
    }
    const focus = spot => ({ x: (spot.x - ox) / fit, y: (spot.y - oy) / fit, radius: 100 / fit })
    if (burst && options.arcs > 0) {
      for (let i = 0; i < 3 && sparks.length < ARCS; i++) {
        const spark = spawnArc(shape, time, focus(burst))
        if (spark) sparks.push(spark)
      }
    }
    burst = null
    if (presence > 0.8 && sparks.length < ARCS) {
      const chance = dt * options.speed * options.arcs
      if (Math.random() < chance * 6 * hover.power * 0.75) {
        const spark = spawnArc(shape, time, focus(hover))
        if (spark) sparks.push(spark)
      } else if (Math.random() < chance * 2.2) {
        const spark = spawnArc(shape, time, null)
        if (spark) sparks.push(spark)
      }
    }
    const arcEnds = new Float32Array(ARCS * 4)
    const arcShape = new Float32Array(ARCS * 4)
    for (let i = 0; i < sparks.length; i++) {
      const spark = sparks[i]
      const offset = i * 4
      const k = (time - spark.born) / spark.life
      arcEnds.set([ox + spark.ax * fit, oy + spark.ay * fit, ox + spark.bx * fit, oy + spark.by * fit], offset)
      arcShape.set([spark.bow * fit, Math.sin(Math.PI * Math.min(1, Math.max(0, k))) * presence, spark.seed, 0], offset)
    }
    let boost = 0
    let flash = 0
    const pulseData = new Float32Array(PULSES * 4)
    for (let i = pulses.length - 1; i >= 0; i--) {
      if ((now - pulses[i].born) / 1000 > 2) pulses.splice(i, 1)
    }
    pulses.forEach((pulse, i) => {
      const age = (now - pulse.born) / 1000
      pulseData.set([pulse.x, pulse.y, age, 1], i * 4)
      boost = Math.max(boost, Math.exp(-age * 1.7))
      flash += Math.exp(-age * 7)
    })

    gl.useProgram(program)
    gl.bindVertexArray(vao)
    gl.uniform2fv(location('uResolution'), [width, height])
    gl.uniform4fv(location('uMapTo'), [ox, oy, fit, shape.glowOffset])
    gl.uniform4fv(location('uSizeTo'), [shape.width, shape.height, shape.glowWidth, shape.glowHeight])
    gl.uniform3fv(location('uHover'), [hover.x, hover.y, hover.power * 0.75])
    gl.uniform4fv(location('uArcEnds[0]'), arcEnds)
    gl.uniform4fv(location('uArcShape[0]'), arcShape)
    gl.uniform4fv(location('uPulses[0]'), pulseData)
    for (const [name, value] of Object.entries({
      uUnit: unit, uTime: time, uPresence: presence, uPulseBoost: boost, uFlash: flash,
    })) gl.uniform1f(location(name), value)
    gl.clear(gl.COLOR_BUFFER_BIT)
    gl.drawArrays(gl.TRIANGLES, 0, 3)
    if (!ready && progress >= 1) {
      ready = true
      options.onReady?.()
    }
    start()
  }

  try {
    for (const [type, source] of [[gl.VERTEX_SHADER, vertex], [gl.FRAGMENT_SHADER, fragment]]) {
      const shader = gl.createShader(type)
      shaders.push(shader)
      gl.shaderSource(shader, source)
      gl.compileShader(shader)
      if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) throw new Error('Electric Logo shader compilation failed')
      gl.attachShader(program, shader)
    }
    gl.linkProgram(program)
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) throw new Error('Electric Logo shader linking failed')
    gl.useProgram(program)
    gl.bindVertexArray(vao)
    for (const [name, values] of [
      ['position', [-1, -1, 3, -1, -1, 3]],
      ['uv', [0, 0, 2, 0, 0, 2]],
    ]) {
      const buffer = gl.createBuffer()
      buffers.push(buffer)
      gl.bindBuffer(gl.ARRAY_BUFFER, buffer)
      gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(values), gl.STATIC_DRAW)
      const attribute = gl.getAttribLocation(program, name)
      gl.enableVertexAttribArray(attribute)
      gl.vertexAttribPointer(attribute, 2, gl.FLOAT, false, 0, 0)
    }
    gl.disable(gl.DEPTH_TEST)
    gl.disable(gl.BLEND)
    gl.clearColor(0, 0, 0, 0)
    gl.uniform3fv(location('uColor'), hexToRgb(options.color))
    gl.uniform3fv(location('uGlowColor'), hexToRgb(options.glowColor))
    for (const [name, value] of Object.entries({
      uMorph: 1, uIntensity: 1, uGlow: 1, uThickness: 1.5,
      uStrands: options.strands, uBend: options.bend, uCrackle: options.crackle,
      uFlicker: 0.6, uFill: 0, uInk: options.dark ? 0 : 1, uHoverRadius: 100,
    })) gl.uniform1f(location(name), value)

    image = new Image()
    image.crossOrigin = 'anonymous'
    image.decoding = 'async'
    image.onerror = fail
    image.onload = () => {
      if (!alive) return
      try {
        shape = traceShape(image)
        if (!shape) return fail()
        const fields = [
          { data: shape.field, width: shape.width, height: shape.height, format: gl.RED, internal: gl.R16F },
          { data: shape.glow, width: shape.glowWidth, height: shape.glowHeight, format: gl.RG, internal: gl.RG16F },
        ]
        // Both slots use this image; there is no source-morph UI in the header.
        for (const [unit, name] of ['tFieldFrom', 'tGlowFrom', 'tFieldTo', 'tGlowTo'].entries()) {
          const field = fields[unit % 2]
          const texture = gl.createTexture()
          textures.push(texture)
          gl.activeTexture(gl.TEXTURE0 + unit)
          gl.bindTexture(gl.TEXTURE_2D, texture)
          gl.pixelStorei(gl.UNPACK_ALIGNMENT, 1)
          gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR)
          gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR)
          gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE)
          gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE)
          gl.texImage2D(gl.TEXTURE_2D, 0, field.internal, field.width, field.height, 0, field.format, gl.FLOAT, field.data)
          gl.uniform1i(location(name), unit)
        }
        if (gl.getError() !== gl.NO_ERROR) return fail()
        resize()
        start()
      } catch { fail() }
    }
    resizeObserver = new ResizeObserver(resize)
    resizeObserver.observe(container)
    intersectionObserver = new IntersectionObserver(([entry]) => {
      visible = entry.isIntersecting
      if (!visible) {
        cancelAnimationFrame(raf)
        raf = 0
      } else start()
    })
    intersectionObserver.observe(container)
    document.addEventListener('visibilitychange', onVisibility)
    container.addEventListener('pointermove', onMove)
    container.addEventListener('pointerdown', onDown)
    container.addEventListener('pointerleave', onLeave)
    container.addEventListener('pointercancel', onLeave)
    image.src = options.src
    return { dispose }
  } catch {
    dispose()
    return null
  }
}
