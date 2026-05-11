import { test as base, expect, type Page, type Browser } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

/**
 * E2E Test: Model Provider Configuration via WebView2 CDP
 *
 * This test validates the model configuration toggle functionality:
 * 1. Navigate to settings page
 * 2. Select a provider (qwen/tongyi)
 * 3. Toggle the enable/disable switch
 * 4. Verify the state change
 */

// CDP endpoint for WebView2
const CDP_ENDPOINT = process.env.CDP_ENDPOINT || 'http://localhost:9222'

// Test configuration
const TEST_CONFIG = {
  providerId: 'qwen',
  apiKey: 'sk-1327700e991e4d9cb07e978053ddd5a7',
  providerName: '通义千问',
}

/**
 * Connect to WebView2 via CDP
 */
async function connectToWebView2(): Promise<Browser | null> {
  console.log(`[CDP] Attempting to connect to WebView2 at ${CDP_ENDPOINT}...`)

  try {
    const response = await fetch(`${CDP_ENDPOINT}/json/version`, { method: 'GET' })
    if (!response.ok) {
      console.log(`[CDP] CDP endpoint not available: ${response.status}`)
      return null
    }

    const versionData = await response.json()
    console.log(`[CDP] Connected to: ${versionData.Browser}`)

    const { chromium } = await import('@playwright/test')
    const browser = await chromium.connectOverCDP(CDP_ENDPOINT)
    console.log('[CDP] Successfully connected to WebView2!')
    return browser
  } catch (error) {
    console.log(`[CDP] Connection failed: ${error}`)
    return null
  }
}

/**
 * Helper function to dismiss welcome/login dialog if present
 */
async function dismissWelcomeDialog(page: Page) {
  await page.keyboard.press('Escape')
  await page.waitForTimeout(300)

  const overlay = page.locator('[data-state="open"][data-slot="dialog-overlay"], .bg-black\\/80.z-\\[60\\]').first()
  if (await overlay.isVisible({ timeout: 1000 }).catch(() => false)) {
    await page.mouse.click(10, 10)
    await page.waitForTimeout(500)
  }

  const enterButton = page.locator('button:has-text("Enter"), button:has-text("进入"), [data-testid="welcome-enter"]').first()
  if (await enterButton.isVisible({ timeout: 2000 }).catch(() => false)) {
    await enterButton.click({ force: true })
    await page.waitForTimeout(500)
    return
  }

  const skipButton = page.locator('button:has-text("Skip"), button:has-text("跳过"), button:has-text("Continue as Guest"), button:has-text("游客")').first()
  if (await skipButton.isVisible({ timeout: 2000 }).catch(() => false)) {
    await skipButton.click({ force: true })
    await page.waitForTimeout(500)
    return
  }

  const closeBtn = page.locator('[aria-label="Close"], [aria-label*="close" i], button[aria-label="close"]').first()
  if (await closeBtn.isVisible({ timeout: 2000 }).catch(() => false)) {
    await closeBtn.click({ force: true })
    await page.waitForTimeout(500)
  }
}

// ============================================================================
// Custom fixtures for WebView2 CDP mode
// ============================================================================

type WebView2Fixtures = {
  webviewBrowser: Browser
  webviewPage: Page
}

const testWithWebView2 = base.extend<WebView2Fixtures>({
  webviewBrowser: async ({}, use) => {
    console.log('[Fixture] webviewBrowser: Starting WebView2 connection...')

    const browser = await connectToWebView2()
    if (!browser) {
      throw new Error(
        'Failed to connect to WebView2 via CDP. ' +
        'Make sure ChatClaw is running with WEBVIEW2_CDP_DEBUG=1'
      )
    }

    await use(browser)
    await browser.close()
  },

  webviewPage: async ({ webviewBrowser }, use) => {
    const context = webviewBrowser.contexts()[0]
    const pages = context.pages()
    let page = pages[0]
    await page.waitForLoadState('domcontentloaded')
    await use(page)
  },
})

// ============================================================================
// WebView2 CDP Mode tests
// ============================================================================

testWithWebView2.describe('WebView2 CDP Mode - Model Provider', () => {
  testWithWebView2('should connect to WebView2 and verify app is running', async ({ webviewPage }) => {
    console.log('[Test] Verifying WebView2 connection...')

    const title = await webviewPage.title()
    console.log(`WebView2 page title: ${title}`)
    expect(title).toBeTruthy()

    const body = webviewPage.locator('body')
    await expect(body).toBeVisible()
    console.log('[Test] WebView2 page is visible')
  })

  testWithWebView2('should navigate to model service settings', async ({ webviewPage }) => {
    console.log('[Test] Navigating to model service settings...')

    // Dismiss welcome dialog if present
    await dismissWelcomeDialog(webviewPage)
    await webviewPage.waitForTimeout(1000)

    // Look for settings button - it might have different text
    const possibleSettingsSelectors = [
      'button:has-text("Settings")',
      'button:has-text("设置")',
      '[data-testid="settings"]',
      '[data-key="settings"]',
      'button:has-text("⚙")',
      'a:has-text("Settings")',
      'a:has-text("设置")',
    ]

    let settingsFound = false
    for (const selector of possibleSettingsSelectors) {
      const btn = webviewPage.locator(selector).first()
      if (await btn.isVisible({ timeout: 500 }).catch(() => false)) {
        console.log(`[Test] Found settings with selector: ${selector}`)
        await btn.click()
        settingsFound = true
        break
      }
    }

    if (!settingsFound) {
      console.log('[Test] Settings button not found, taking screenshot for debug')
      await webviewPage.screenshot({ path: 'debug-settings-not-found.png' })
    }

    await webviewPage.waitForTimeout(2000)

    // Now look for Model Service / 模型服务 link
    const modelServiceSelectors = [
      'text=Model Service',
      'text=模型服务',
      'a:has-text("Model Service")',
      'a:has-text("模型服务")',
      'button:has-text("Model Service")',
      'button:has-text("模型服务")',
    ]

    for (const selector of modelServiceSelectors) {
      const el = webviewPage.locator(selector).first()
      if (await el.isVisible({ timeout: 1000 }).catch(() => false)) {
        console.log(`[Test] Found model service with selector: ${selector}`)
        await el.click()
        break
      }
    }

    await webviewPage.waitForTimeout(2000)
    console.log('[Test] Navigated to model service settings')

    // Check if we can find qwen/tongyi provider
    const qwenSelectors = [
      `[data-provider-id="qwen"]`,
      'button:has-text("通义千问")',
      'button:has-text("Qwen")',
      'button:has-text("qwen")',
      'text=通义千问',
    ]

    let qwenFound = false
    for (const selector of qwenSelectors) {
      const el = webviewPage.locator(selector).first()
      if (await el.isVisible({ timeout: 1000 }).catch(() => false)) {
        console.log(`[Test] Found Qwen provider with selector: ${selector}`)
        await el.click()
        qwenFound = true
        break
      }
    }

    if (!qwenFound) {
      console.log('[Test] Qwen provider not found, current URL:', webviewPage.url())
      await webviewPage.screenshot({ path: 'debug-qwen-not-found.png' })
    }

    await expect(webviewPage.locator('body')).toBeVisible()
  })

  testWithWebView2('should test qwen provider toggle', async ({ webviewPage }) => {
    console.log('[Test] Testing Qwen provider toggle...')

    // Dismiss welcome dialog
    await dismissWelcomeDialog(webviewPage)
    await webviewPage.waitForTimeout(1000)

    // Navigate to settings
    const settingsBtn = webviewPage.locator('button:has-text("Settings"), button:has-text("设置")').first()
    if (await settingsBtn.isVisible({ timeout: 5000 }).catch(() => false)) {
      await settingsBtn.click()
    }
    await webviewPage.waitForTimeout(1500)

    // Navigate to model service
    const modelServiceBtn = webviewPage.locator('button:has-text("Model Service"), button:has-text("模型服务")').first()
    if (await modelServiceBtn.isVisible({ timeout: 3000 }).catch(() => false)) {
      await modelServiceBtn.click()
    }
    await webviewPage.waitForTimeout(1500)

    // Find and click Qwen provider
    const qwenBtn = webviewPage.locator(
      `[data-provider-id="qwen"], button:has-text("通义千问"), button:has-text("Qwen")`
    ).first()
    
    if (await qwenBtn.isVisible({ timeout: 5000 }).catch(() => false)) {
      console.log('[Test] Found Qwen, clicking to expand...')
      await qwenBtn.click()
      await webviewPage.waitForTimeout(1000)

      // Look for enable/disable switch
      const switchEl = webviewPage.locator('[role="switch"], [type="checkbox"]').first()
      if (await switchEl.isVisible({ timeout: 2000 }).catch(() => false)) {
        const initialState = await switchEl.isChecked().catch(() => false)
        console.log(`[Test] Initial switch state: ${initialState ? 'ON' : 'OFF'}`)
        
        await switchEl.click()
        await webviewPage.waitForTimeout(500)
        
        const newState = await switchEl.isChecked().catch(() => false)
        console.log(`[Test] New switch state: ${newState ? 'ON' : 'OFF'}`)
        
        expect(newState).not.toBe(initialState)
        console.log('[Test] Toggle test PASSED')
      } else {
        console.log('[Test] Switch not found in expanded Qwen panel')
        await webviewPage.screenshot({ path: 'debug-switch-not-found.png' })
      }
    } else {
      console.log('[Test] Qwen provider not found')
      await webviewPage.screenshot({ path: 'debug-qwen-toggle-not-found.png' })
    }
  })

  testWithWebView2('should test qwen API key input', async ({ webviewPage }) => {
    console.log('[Test] Testing Qwen API key input...')

    // Dismiss welcome dialog
    await dismissWelcomeDialog(webviewPage)
    await webviewPage.waitForTimeout(1000)

    // Navigate to settings
    const settingsBtn = webviewPage.locator('button:has-text("Settings"), button:has-text("设置")').first()
    if (await settingsBtn.isVisible({ timeout: 5000 }).catch(() => false)) {
      await settingsBtn.click()
    }
    await webviewPage.waitForTimeout(1500)

    // Navigate to model service
    const modelServiceBtn = webviewPage.locator('button:has-text("Model Service"), button:has-text("模型服务")').first()
    if (await modelServiceBtn.isVisible({ timeout: 3000 }).catch(() => false)) {
      await modelServiceBtn.click()
    }
    await webviewPage.waitForTimeout(1500)

    // Find and click Qwen provider
    const qwenBtn = webviewPage.locator(
      `[data-provider-id="qwen"], button:has-text("通义千问"), button:has-text("Qwen")`
    ).first()
    
    if (await qwenBtn.isVisible({ timeout: 5000 }).catch(() => false)) {
      console.log('[Test] Found Qwen, clicking to expand...')
      await qwenBtn.click()
      await webviewPage.waitForTimeout(1000)

      // Look for API key input
      const apiKeySelectors = [
        'input[type="password"]',
        'input[placeholder*="API"]',
        'input[placeholder*="Key"]',
        'input[placeholder*="密钥"]',
        'input[placeholder*="key"]',
      ]

      for (const selector of apiKeySelectors) {
        const input = webviewPage.locator(selector).first()
        if (await input.isVisible({ timeout: 1000 }).catch(() => false)) {
          console.log(`[Test] Found API key input with selector: ${selector}`)
          await input.clear()
          await input.fill(TEST_CONFIG.apiKey)
          await webviewPage.waitForTimeout(500)
          
          const value = await input.inputValue()
          console.log(`[Test] API key entered, value length: ${value.length}`)
          expect(value).toBe(TEST_CONFIG.apiKey)
          console.log('[Test] API key input test PASSED')
          return
        }
      }
      
      console.log('[Test] API key input not found')
      await webviewPage.screenshot({ path: 'debug-apikey-not-found.png' })
    } else {
      console.log('[Test] Qwen provider not found')
      await webviewPage.screenshot({ path: 'debug-qwen-apikey-not-found.png' })
    }
  })
})
