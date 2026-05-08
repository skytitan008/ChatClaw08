/**
 * Global teardown: Stop ChatClaw
 */

import { spawn } from 'child_process'

export default async function globalTeardown() {
  const pid = process.env.CHATCLAW_PID
  if (pid) {
    console.log(`[teardown] Stopping ChatClaw (PID: ${pid})...`)
    try {
      // Use taskkill on Windows
      spawn('taskkill', ['/F', '/PID', pid], { windowsHide: true })
    } catch (err) {
      console.error('[teardown] Failed to stop ChatClaw:', err)
    }
  }
}
