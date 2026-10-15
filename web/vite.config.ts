import { defineConfig } from 'vitest/config'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  build: {
    outDir: '../pkg/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
  },
  resolve: {
    // Vitest resolves package "exports" with node-like conditions, which selects
    // svelte's SERVER build and makes `mount()` throw in a jsdom test. These are
    // Vite's own default CLIENT conditions, so the real build is unchanged.
    conditions: ['module', 'browser', 'development|production'],
  },
  test: {
    environment: 'jsdom',
  },
})
