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

// ESM __dirname equivalent
const __filename = fileURLToPath(import.meta.url)
const __dirname = path.dirname(__filename)

// Calculate project root: frontend/tests/setup → frontend/tests → frontend → project root
const projectRoot = path.resolve(__dirname, '../../..')

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

  console.log('[setup] ==========================================================')
  console.log('[setup] Starting ChatClaw with WebView2 CDP debugging...')
  console.log(`[setup] Project root: ${projectRoot}`)
  console.log(`[setup] EXE path: ${exePath}`)
  console.log('[setup] ==========================================================')

  // Use PowerShell to set env var and launch ChatClaw
  const psCommand = `
    $env:WEBVIEW2_CDP_DEBUG = "1"
    Write-Host "[PowerShell] WEBVIEW2_CDP_DEBUG set to: $env:WEBVIEW2_CDP_DEBUG"
    & "${exePath}"
  `

  console.log('[setup] Launching ChatClaw via PowerShell...')

  const chatclawProc = spawn('powershell', ['-ExecutionPolicy', 'Bypass', '-Command', psCommand], {
    cwd: projectRoot,
    stdio: ['pipe', 'pipe', 'pipe'],
    windowsHide: false,
  })

  // Store PID for teardown
  process.env.CHATCLAW_PID = String(chatclawProc.pid)
  console.log(`[setup] ChatClaw PID: ${chatclawProc.pid}`)

  chatclawProc.stdout?.on('data', (data) => {
    process.stdout.write(`[ChatClaw] ${data}`)
  })

  chatclawProc.stderr?.on('data', (data) => {
    process.stderr.write(`[ChatClaw] ${data}`)
  })

  chatclawProc.on('error', (err) => {
    console.error('[setup] ERROR: Failed to start ChatClaw:', err)
    process.exit(1)
  })

  chatclawProc.on('exit', (code, signal) => {
    console.log(`[setup] ChatClaw exited with code ${code}, signal ${signal}`)
  })

  console.log('[setup] Waiting for CDP to become available...')

  const cdpAvailable = await checkCDPAvailable()
  if (!cdpAvailable) {
    console.error('[setup] ERROR: CDP not available after 60 seconds timeout')
    console.error('[setup] Make sure ChatClaw.exe is built: go build -o bin/ChatClaw.exe .')
    chatclawProc.kill()
    process.exit(1)
  }

  console.log('[setup] ==========================================================')
  console.log('[setup] SUCCESS: ChatClaw is ready for WebView2 CDP testing!')
  console.log('[setup] ==========================================================')
}
