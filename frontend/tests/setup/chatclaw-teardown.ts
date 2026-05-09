/**
 * Global teardown: Stop ChatClaw
 */

import { spawn } from 'child_process'

export default async function globalTeardown() {
  const pid = process.env.CHATCLAW_PID
  console.log(`[teardown] Starting teardown, stored PID: ${pid}`)

  if (pid) {
    try {
      console.log(`[teardown] Attempting to kill process ${pid}...`)
      spawn('taskkill', ['/F', '/PID', pid], { windowsHide: true })
      console.log(`[teardown] Kill command sent for PID ${pid}`)
    } catch (err) {
      console.error('[teardown] Failed to stop ChatClaw:', err)
    }
  } else {
    console.log('[teardown] No PID stored, trying to find ChatClaw processes...')
    // Try to kill any remaining ChatClaw processes
    spawn('taskkill', ['/F', '/IM', 'ChatClaw.exe'], { windowsHide: true })
  }

  // Wait a bit for cleanup
  await new Promise(resolve => setTimeout(resolve, 1000))
  console.log('[teardown] Teardown complete')
}
