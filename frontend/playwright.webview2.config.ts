/**
 * Playwright Configuration for WebView2 (Real App) Testing
 *
 * This configuration auto-starts:
 * 1. Frontend dev server on port 9245 (via webServer)
 * 2. ChatClaw.exe with CDP debugging enabled (via globalSetup)
 *
 * Usage:
 *   pnpm playwright test --config playwright.webview2.config.ts
 *
 * CDP Connection:
 *   - ChatClaw auto-started with WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS="--remote-debugging-port=9222"
 *   - Playwright connects via chromium.connectOverCDP('http://localhost:9222')
 */

import { defineConfig, devices } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const projectRoot = path.resolve(__dirname, '../..')

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: 'list',
  use: {
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    timeout: 60000,
  },
  projects: [
    {
      name: 'webview2',
      use: {
        ...devices['Desktop Chrome'],
      },
    },
  ],
  // Start frontend dev server only (ChatClaw is started by globalSetup)
  webServer: {
    command: 'npm run dev -- --port 9245',
    url: 'http://localhost:9245',
    reuseExistingServer: true,
    timeout: 120000,
    stdout: 'pipe',
    stderr: 'pipe',
    cwd: './',
  },
  globalSetup: './tests/setup/chatclaw-launcher.ts',
  globalTeardown: './tests/setup/chatclaw-teardown.ts',
  timeout: 120000,
  expect: {
    timeout: 30000,
  },
})
