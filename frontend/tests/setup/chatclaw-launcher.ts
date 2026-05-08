/**
 * Global setup: Start ChatClaw with WebView2 CDP debugging enabled
 *
 * Uses the newly added WEBVIEW2_CDP_DEBUG=1 environment variable
 * which enables CDP debugging in the Go code.
 */

import { spawn } from 'child_process'
import { setTimeout as sleep } from 'timers/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const projectRoot = path.resolve(__dirname, '../../..')  // tests/setup → tests → frontend → project root

async function checkCDPAvailable(retries = 60): Promise<boolean> {
  for (let i = 0; i < retries; i++) {
    try {
      const response = await fetch('http://localhost:9222/json/version', { method: 'GET' })
      if (response.ok) {
        const data = await response.json()
        console.log(`[setup] CDP available: ${data.Browser}`)
        return true
      }
    } catch {
      // CDP not ready yet
    }
    await sleep(1000)
    if (i < 10 || i % 10 === 0) {
      console.log(`[setup] Waiting for CDP... (${i + 1}/${retries})`)
    }
  }
  return false
}

export default async function globalSetup() {
  const exePath = path.join(projectRoot, 'bin/ChatClaw.exe')

  console.log('[setup] Starting ChatClaw with WebView2 CDP debugging...')
  console.log(`[setup] Project root: ${projectRoot}`)
  console.log(`[setup] EXE path: ${exePath}`)

  // Use PowerShell to set env var and launch ChatClaw
  const psCommand = `
    $env:WEBVIEW2_CDP_DEBUG = "1"
    Write-Host "Set WEBVIEW2_CDP_DEBUG to: $env:WEBVIEW2_CDP_DEBUG"
    & "${exePath}"
  `

  const chatclawProc = spawn('powershell', ['-ExecutionPolicy', 'Bypass', '-Command', psCommand], {
    cwd: projectRoot,
    stdio: ['pipe', 'pipe', 'pipe'],
    windowsHide: true,
  })

  chatclawProc.stdout?.on('data', (data) => {
    process.stdout.write(`[ChatClaw] ${data}`)
  })

  chatclawProc.stderr?.on('data', (data) => {
    process.stderr.write(`[ChatClaw] ${data}`)
  })

  chatclawProc.on('error', (err) => {
    console.error('[setup] Failed to start ChatClaw:', err)
    process.exit(1)
  })

  console.log('[setup] ChatClaw launched, waiting for CDP...')

  const cdpAvailable = await checkCDPAvailable()
  if (!cdpAvailable) {
    console.error('[setup] CDP not available after timeout')
    chatclawProc.kill()
    process.exit(1)
  }

  console.log('[setup] ChatClaw is ready for WebView2 CDP testing!')
}
