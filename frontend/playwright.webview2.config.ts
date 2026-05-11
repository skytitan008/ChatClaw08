/**
 * Playwright Configuration for WebView2 (Real App) Testing
 *
 * IMPORTANT: This configuration uses globalSetup to automatically start ChatClaw with CDP enabled.
 *
 * CDP Connection:
 *   - globalSetup launches ChatClaw with WEBVIEW2_CDP_DEBUG=1
 *   - Playwright connects via CDP to the running WebView2
 */

import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: [['list']],
  globalSetup: './tests/setup/chatclaw-launcher.ts',
  globalTeardown: './tests/setup/chatclaw-teardown.ts',
  use: {
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    // Don't launch browser - we'll connect to existing WebView2 via CDP
    launchOptions: {
      args: ['--disable-launch-browser'],
    },
  },
  projects: [
    {
      name: 'webview2',
      use: {
        ...devices['Desktop Chrome'],
      },
    },
  ],
  timeout: 120000,
  expect: {
    timeout: 30000,
  },
})
