<script setup lang="ts">
import { ref, onMounted, onUnmounted, watch, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import * as TerminalService from '@bindings/chatclaw/internal/services/terminal/terminalservice'
import { Loader2 } from 'lucide-vue-next'

const { t } = useI18n()

const terminalContainer = ref<HTMLDivElement | null>(null)
const toolStatusEl = ref<HTMLDivElement | null>(null)

let terminal: Terminal | null = null
let fitAddon: FitAddon | null = null
let currentSessionId = ''
let currentWorkDir = ''
let isCommandRunning = false
let commandBuffer = ''
let cursorPosition = 0

// Command history
const commandHistory: string[] = []
let historyIndex = -1
let historySaveIndex = -1

// Tab completion - common commands
const commonCommands = [
  'openclaw', 'clear', 'cls', 'cd', 'dir', 'ls', 'pwd', 'exit',
  'node', 'npm', 'npx', 'bun', 'python', 'pip', 'git', 'codex',
]

const toolStatuses = ref<{ name: string; installed: boolean; version: string }[]>([])
const isLoading = ref(true)

// Initialize terminal
const initTerminal = async () => {
  if (!terminalContainer.value) return

  terminal = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: '"Cascadia Code", "Fira Code", "Consolas", monospace',
    theme: {
      background: '#0d1117',
      foreground: '#c9d1d9',
      cursor: '#c9d1d9',
      black: '#0d1117',
      red: '#f85149',
      green: '#3fb950',
      yellow: '#d29922',
      blue: '#58a6ff',
      magenta: '#bc8cff',
      cyan: '#39c5cf',
      white: '#c9d1d9',
      brightBlack: '#484f58',
      brightRed: '#ffa198',
      brightGreen: '#56d364',
      brightYellow: '#e3b341',
      brightBlue: '#79c0ff',
      brightMagenta: '#d2a8ff',
      brightCyan: '#56d4dd',
      brightWhite: '#ffffff',
    },
    scrollback: 10000,
    allowProposedApi: true,
    convertEol: true, // Ensure \n moves cursor to column 0 of new line
  })

  fitAddon = new FitAddon()
  terminal.loadAddon(fitAddon)
  terminal.open(terminalContainer.value)

  // Focus the terminal
  terminal.focus()

  // Fit terminal to container
  await nextTick()
  try {
    fitAddon.fit()
  } catch (e) {
    console.warn('Terminal fit failed:', e)
  }

  // Create session
  await createSession()

  // Load tool status
  await loadToolStatus()

  isLoading.value = false

  // Handle input - use onData for character input
  terminal.onData(async (data: string) => {
    await handleTerminalInput(data)
  })

  // Handle resize
  terminal.onResize(({ cols, rows }) => {
    // Could send resize event to backend if needed
  })

  // Click to focus terminal
  terminalContainer.value?.addEventListener('click', () => {
    terminal?.focus()
  })
}

// Create terminal session
const createSession = async () => {
  try {
    const session = await TerminalService.CreateSession()
    if (!session) {
      terminal?.writeln('\x1b[31mFailed to create session: no session returned\x1b[0m')
      return
    }
    currentSessionId = session.id
    currentWorkDir = session.workDir || ''

    // Show welcome message
    terminal?.writeln('')
    terminal?.writeln('\x1b[36m╔═══════════════════════════════════════════════════════════╗\x1b[0m')
    terminal?.writeln('\x1b[36m║\x1b[0m  \x1b[1;37mOpenClaw Terminal\x1b[0m                                    \x1b[36m║\x1b[0m')
    terminal?.writeln('\x1b[36m║\x1b[0m  Commands: openclaw, npm, npx, codex, uv, bun, bunx    \x1b[36m║\x1b[0m')
    terminal?.writeln('\x1b[36m║\x1b[0m  Shortcuts: cd openclaw: | cd ~ | cd ..                      \x1b[36m║\x1b[0m')
    terminal?.writeln('\x1b[36m╚═══════════════════════════════════════════════════════════╝\x1b[0m')
    terminal?.writeln('')
    prompt()
  } catch (e) {
    terminal?.writeln(`\x1b[31mFailed to create session: ${e}\x1b[0m`)
  }
}

// Load tool status
const loadToolStatus = async () => {
  try {
    const status = await TerminalService.GetToolStatus()
    toolStatuses.value = status.map((s) => ({
      name: s.name,
      installed: s.installed,
      version: s.installed_version || '',
    }))
  } catch (e) {
    console.error('Failed to load tool status:', e)
  }
}

// Show prompt
// Ensure cursor is at column 0 of a new line
const resetCursor = () => {
  // Move to new line and column 0
  terminal?.write('\r\n')
}

const prompt = () => {
  const shortDir = getShortPath(currentWorkDir)
  // Ensure we're at column 0 of a new line
  resetCursor()
  terminal?.write(`\x1b[32m${shortDir}\x1b[0m $ `)
}

// Get short path for display
const getShortPath = (path: string): string => {
  const home = getHomeDir()
  if (path.startsWith(home)) {
    return '~' + path.slice(home.length).replace(/\\/g, '/')
  }
  return path.replace(/\\/g, '/')
}

// Get home directory - use backend provided path or fallback
const getHomeDir = (): string => {
  // Try to get from backend session's home directory
  // For now, use a reasonable Windows fallback
  return 'C:\\Users'
}

// Detect if we're on Windows (Wails runs in WebView on Windows)
const isWindows = (): boolean => {
  // Wails on Windows uses WebView2, check user agent
  return typeof navigator !== 'undefined' && /windows/i.test(navigator.userAgent)
}

// Handle terminal input
const handleTerminalInput = async (data: string) => {
  if (!terminal) return

  // Handle Ctrl+C
  if (data === '\x03') {
    terminal.write('^C')
    prompt()
    commandBuffer = ''
    cursorPosition = 0
    isCommandRunning = false
    return
  }

  // Ctrl+L (clear)
  if (data === '\x0c') {
    terminal.write('\x1b[2J\x1b[H')
    return
  }

  const code = data.charCodeAt(0)

  // Enter
  if (code === 13) {
    // Write the command to terminal first
    terminal.write('\r\n')

    const cmd = commandBuffer
    commandBuffer = ''
    cursorPosition = 0

    executeCommand(cmd)
    return
  }

  // Backspace
  if (code === 127 || code === 8) {
    if (commandBuffer.length > 0 && cursorPosition > 0) {
      commandBuffer = commandBuffer.slice(0, cursorPosition - 1) + commandBuffer.slice(cursorPosition)
      cursorPosition--
      terminal.write('\b \b')
      const remaining = commandBuffer.slice(cursorPosition)
      if (remaining) {
        terminal.write(remaining)
        terminal.write(' ')
        for (let i = 0; i < remaining.length + 1; i++) {
          terminal.write('\b')
        }
      }
    }
    return
  }

  // Arrow keys - command history
  if (data === '\x1b[A') {
    // Up arrow - previous command
    if (historyIndex < commandHistory.length - 1) {
      historyIndex++
      clearCommandLine()
      commandBuffer = commandHistory[commandHistory.length - 1 - historyIndex]
      cursorPosition = commandBuffer.length
      terminal.write(commandBuffer)
    }
    return
  }
  if (data === '\x1b[B') {
    // Down arrow - next command
    if (historyIndex > 0) {
      historyIndex--
      clearCommandLine()
      commandBuffer = commandHistory[commandHistory.length - 1 - historyIndex]
      cursorPosition = commandBuffer.length
      terminal.write(commandBuffer)
    } else if (historyIndex === 0) {
      historyIndex = -1
      clearCommandLine()
      commandBuffer = ''
      cursorPosition = 0
    }
    return
  }

  // Tab - command/path completion
  if (code === 9) {
    if (commandBuffer.length > 0) {
      const completion = await getCompletion(commandBuffer)
      if (completion) {
        clearCommandLine()
        commandBuffer = completion
        cursorPosition = commandBuffer.length
        terminal.write(commandBuffer)
      }
    }
    return
  }

  // Printable characters - write to terminal and buffer
  if (code >= 32) {
    commandBuffer = commandBuffer.slice(0, cursorPosition) + data + commandBuffer.slice(cursorPosition)
    cursorPosition++
    terminal.write(data)
  }
}

// Clear current command line from terminal
const clearCommandLine = () => {
  if (!terminal) return
  // Move cursor to beginning
  for (let i = 0; i < cursorPosition; i++) {
    terminal.write('\b')
  }
  // Overwrite with spaces
  for (let i = 0; i < commandBuffer.length; i++) {
    terminal.write(' ')
  }
  // Move cursor back to beginning
  for (let i = 0; i < commandBuffer.length; i++) {
    terminal.write('\b')
  }
}

// Get tab completion for current input
const getCompletion = async (input: string): Promise<string | null> => {
  // Check if we're completing a path (after command and space)
  const lastSpaceIndex = input.lastIndexOf(' ')
  const lastToken = lastSpaceIndex >= 0 ? input.slice(lastSpaceIndex + 1) : input

  // If the last token contains path separators, do path completion
  if (lastToken.includes('/') || lastToken.includes('\\')) {
    return getPathCompletion(lastToken, lastSpaceIndex >= 0 ? input.slice(0, lastSpaceIndex + 1) : '')
  }

  // Otherwise, do command completion
  const matches = commonCommands.filter(cmd => cmd.startsWith(lastToken))
  if (matches.length === 1) {
    // Replace just the last token
    if (lastSpaceIndex >= 0) {
      return input.slice(0, lastSpaceIndex + 1) + matches[0]
    }
    return matches[0]
  } else if (matches.length > 1) {
    // Show all matches
    terminal?.write('\r\n')
    matches.forEach(m => terminal?.write(m + '  '))
    terminal?.write('\r\n')
    prompt()
    terminal?.write(input)
    return null // Don't change input
  }
  return null
}

// Get path completion from backend
const getPathCompletion = async (partialPath: string, prefix: string): Promise<string | null> => {
  try {
    const result = await TerminalService.GetPathCompletion(currentSessionId, partialPath)
    if (!result?.matches || result.matches.length === 0) {
      return null
    }
    if (result.matches.length === 1) {
      // Single match - complete it
      return prefix + result.matches[0]
    } else {
      // Multiple matches - show them
      terminal?.write('\r\n')
      result.matches.forEach(m => terminal?.write(m + '  '))
      terminal?.write('\r\n')
      prompt()
      terminal?.write(prefix + partialPath)
      return null
    }
  } catch {
    return null
  }
}

// Execute command
const executeCommand = async (cmd: string) => {
  const trimmedCmd = cmd.trim()
  if (!trimmedCmd) {
    prompt()
    return
  }

  // Check for built-in commands that need special handling
  const parts = trimmedCmd.split(/\s+/)
  const baseCmd = parts[0].toLowerCase()

  if (baseCmd === 'clear' || baseCmd === 'cls') {
    terminal?.write('\x1b[2J\x1b[H')
    prompt()
    return
  }

  // Save command to history (only non-empty commands)
  if (trimmedCmd && (commandHistory.length === 0 || commandHistory[commandHistory.length - 1] !== trimmedCmd)) {
    commandHistory.push(trimmedCmd)
    historyIndex = -1
    historySaveIndex = commandHistory.length
  }

  isCommandRunning = true

  try {
    const result = await TerminalService.ExecuteCommand(currentSessionId, trimmedCmd)

    // Write output - normalize line endings and ensure proper cursor positioning
    if (result?.stdout) {
      // Normalize \r\n to just \n for consistent handling
      let output = result.stdout.replace(/\r\n/g, '\n').replace(/\r/g, '\n')
      // Remove trailing whitespace/newlines to avoid double newlines
      output = output.replace(/[ \t]+\n/g, '\n')
      terminal?.write(output)
    }
    if (result?.stderr) {
      terminal?.write(`\x1b[31m${result.stderr.replace(/\r\n/g, '\n').replace(/\r/g, '\n')}\x1b[0m`)
    }

    // Handle cd command - update working directory
    if (baseCmd === 'cd' && result?.exitCode === 0) {
      try {
        const session = await TerminalService.GetSession(currentSessionId)
        if (session?.workDir) {
          currentWorkDir = session.workDir
        }
      } catch {
        // Ignore errors
      }
    }
  } catch (e) {
    terminal?.writeln(`\x1b[31mError: ${e}\x1b[0m`)
  }

  isCommandRunning = false
  prompt()
}

// Handle window resize
const handleResize = () => {
  if (fitAddon) {
    try {
      fitAddon.fit()
    } catch (e) {
      console.warn('Terminal fit failed on resize:', e)
    }
  }
}

// Resize observer for terminal container
let resizeObserver: ResizeObserver | null = null

onMounted(() => {
  initTerminal()

  // Set up resize observer
  if (terminalContainer.value) {
    resizeObserver = new ResizeObserver(() => {
      handleResize()
    })
    resizeObserver.observe(terminalContainer.value)
  }

  // Handle window resize
  window.addEventListener('resize', handleResize)
})

onUnmounted(() => {
  window.removeEventListener('resize', handleResize)
  resizeObserver?.disconnect()

  if (terminal) {
    terminal.dispose()
    terminal = null
  }

  // Close session
  if (currentSessionId) {
    TerminalService.CloseSession(currentSessionId).catch(() => {})
  }
})
</script>

<template>
  <div class="flex h-full w-full flex-col overflow-hidden bg-background text-foreground">
    <!-- Tool status bar -->
    <div
      ref="toolStatusEl"
      class="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-border bg-muted/30 px-4 py-2 text-xs"
    >
      <span class="shrink-0 font-medium text-foreground">{{ t('openclaw.terminal.tools') }}:</span>
      <div class="flex flex-wrap items-center gap-x-3">
        <div
          v-for="tool in toolStatuses"
          :key="tool.name"
          class="flex items-center gap-1"
        >
          <span
            class="size-1.5 rounded-full"
            :class="tool.installed ? 'bg-green-500' : 'bg-muted-foreground'"
          />
          <span :class="tool.installed ? 'text-foreground' : 'text-muted-foreground'">
            {{ tool.name }}
          </span>
          <span v-if="tool.version" class="text-muted-foreground">({{ tool.version }})</span>
        </div>
        <div v-if="isLoading" class="flex items-center gap-1 text-muted-foreground">
          <Loader2 class="size-3 animate-spin" />
          {{ t('openclaw.terminal.loading') }}
        </div>
      </div>
    </div>

    <!-- Terminal container -->
    <div class="relative flex-1 overflow-hidden">
      <!-- Loading overlay -->
      <div
        v-if="isLoading"
        class="absolute inset-0 z-10 flex items-center justify-center bg-background/80"
      >
        <Loader2 class="size-8 animate-spin text-muted-foreground" />
      </div>

      <!-- Terminal -->
      <div
        ref="terminalContainer"
        class="h-full w-full"
      />
    </div>
  </div>
</template>

<style>
.xterm {
  height: 100%;
  padding: 8px;
}

.xterm-viewport {
  overflow-y: auto !important;
}

.xterm-screen {
  height: 100%;
}
</style>
