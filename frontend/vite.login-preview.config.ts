import { defineConfig, mergeConfig } from 'vite'
import config from './vite.config'

export default defineConfig((environment) => {
  const base = typeof config === 'function' ? config(environment) : config
  return mergeConfig(base, {
    plugins: [{
      name: 'login-design-read-only',
      configureServer(server) {
        server.middlewares.use((request, response, next) => {
          const pathname = (request.url || '').split('?')[0]
          if (request.method === 'GET' && pathname === '/setup/status') {
            response.setHeader('Content-Type', 'application/json')
            response.end(JSON.stringify({ code: 0, data: { needs_setup: false, step: 'complete' } }))
            return
          }
          if (/^\/(api|v1|setup)(\/|$)/.test(pathname) &&
            !(request.method === 'GET' && pathname === '/api/v1/settings/public')) {
            response.statusCode = 403
            response.setHeader('Content-Type', 'application/json')
            response.end(JSON.stringify({ message: 'Local design preview: authentication and writes disabled' }))
            return
          }
          next()
        })
      },
    }],
    server: { host: '127.0.0.1', port: 4192, strictPort: true },
  })
})
