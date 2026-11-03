export function isLocalHomeStylePreview(
  dev: boolean, hostname: string, search: string, preview = 'styles',
): boolean {
  return dev &&
    ['localhost', '127.0.0.1', '[::1]'].includes(hostname) &&
    new URLSearchParams(search).get('preview') === preview
}
