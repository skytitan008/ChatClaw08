<script setup lang="ts">
/**
 * Left-side navigation with system switcher (ChatClaw / OpenClaw).
 *
 * Menu items are fully computed from `currentSystem`:
 *  - Both: assistant, knowledge, scheduled-tasks, skills (native vs openclaw-skills), channels, tools, multiask
 *  - chatclaw only: hides memory
 *  - openclaw only: shows memory
 *
 * Each nav item may specify a `systemModuleMap` to resolve to a different
 * NavModule per system. Example:
 *   { key: 'assistant', systemModuleMap: { openclaw: 'openclaw' } }
 * When the user clicks the item, we look up the effective module from the map
 * (falling back to `key`) and open the tab with `systemOwner` captured.
 */

type SvgComponent = any
import { ref, computed, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  useNavigationStore,
  useAppStore,
  useOpenClawGatewayStore,
  type NavModule,
  type SystemOwner,
} from '@/stores'
import * as ToolchainService from '@bindings/chatclaw/internal/services/toolchain/toolchainservice'
import { cn } from '@/lib/utils'
import IconAssistant from '@/assets/icons/assistant.svg'
import IconKnowledge from '@/assets/icons/knowledge.svg'
import IconTools from '@/assets/icons/tools.svg'
import IconCorn from '@/assets/icons/corn.svg'
import IconSkills from '@/assets/icons/skills.svg'
import IconMemory from '@/assets/icons/memory.svg'
import IconMultiask from '@/assets/icons/multiask.svg'
import IconChannels from '@/assets/icons/channels.svg'
import IconSettings from '@/assets/icons/settings.svg'
import IconChatClaw from '@/assets/icons/chatclaw.svg'
import IconOpenClaw from '@/assets/icons/openclaw-logo.svg'
import IconTerminal from '@/assets/icons/terminal.svg'
import { Check } from 'lucide-vue-next'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import ChatWikiSidebarAccountCard from './ChatWikiSidebarAccountCard.vue'
import SideNavSystemSwitcherButton from './SideNavSystemSwitcherButton.vue'
import OpenClawGatewayStatusPillContent from './OpenClawGatewayStatusPillContent.vue'

const { t } = useI18n()
const navigationStore = useNavigationStore()
const appStore = useAppStore()
const gatewayStore = useOpenClawGatewayStore()

watch(
  () => appStore.currentSystem,
  (sys) => {
    if (sys === 'openclaw') {
      void gatewayStore.poll()
      gatewayStore.startHeartbeat()
    } else {
      gatewayStore.stopHeartbeat()
    }
  },
  { immediate: true }
)

const switcherOpen = ref(false)
const switcherButtonCmp = ref<InstanceType<typeof SideNavSystemSwitcherButton> | null>(null)
const triggerRef = computed(
  () => switcherButtonCmp.value?.buttonRef ?? null
)
const dropdownRef = ref<HTMLDivElement | null>(null)
const dropdownStyle = ref({ top: '0px', left: '0px', minWidth: '0px' })

const updateDropdownPosition = async () => {
  const el = triggerRef.value
  if (!el) return
  await nextTick()
  const rect = el.getBoundingClientRect()
  dropdownStyle.value = {
    top: `${rect.bottom + 4}px`,
    left: `${rect.left}px`,
    minWidth: `${Math.max(rect.width, 128)}px`,
  }
}

watch(switcherOpen, (open) => {
  if (open) updateDropdownPosition()
})

// Close dropdown when clicking outside the trigger or dropdown panel.
// Using a native mousedown listener avoids the full-screen overlay that was
// previously blocking all clicks (including Wails native drag-region events).
const handleOutsideMouseDown = (e: MouseEvent) => {
  if (!switcherOpen.value) return
  const target = e.target as Node
  if (triggerRef.value?.contains(target)) return
  if (dropdownRef.value?.contains(target)) return
  switcherOpen.value = false
}

onMounted(() => document.addEventListener('mousedown', handleOutsideMouseDown, true))
onUnmounted(() => document.removeEventListener('mousedown', handleOutsideMouseDown, true))

interface SystemOption {
  value: SystemOwner
  labelKey: string
  icon: SvgComponent
}

const systemOptions: SystemOption[] = [
  { value: 'chatclaw', labelKey: 'nav.systemChatClaw', icon: IconChatClaw },
  { value: 'openclaw', labelKey: 'nav.systemOpenClaw', icon: IconOpenClaw },
]

const currentOption = computed(
  () => systemOptions.find((o) => o.value === appStore.currentSystem) ?? systemOptions[0]
)

/** Native title on switcher when ChatClaw (OpenClaw uses gateway tooltip on hover). */
const switcherNativeTitle = computed(() => t(currentOption.value.labelKey))

const toggleSwitcher = () => {
  switcherOpen.value = !switcherOpen.value
}

const selectSystem = (system: SystemOwner) => {
  if (system === appStore.currentSystem) {
    switcherOpen.value = false
    return
  }
  appStore.setCurrentSystem(system)
  const switchToSystem = async () => {
    if (system === 'openclaw') {
      try {
        const status = await ToolchainService.GetOpenClawRuntimeStatus()
        if (!status?.installed) {
          navigationStore.resetToSingleTab('openclaw-runtime-environment', system)
          return
        }
      } catch {
        // Treat unknown status as not installed to keep behavior safe and explicit.
        navigationStore.resetToSingleTab('openclaw-runtime-environment', system)
        return
      }
      navigationStore.resetToSingleTab('openclaw', system)
      return
    }
    navigationStore.resetToSingleTab('assistant', system)
  }
  void switchToSystem()
  switcherOpen.value = false
}

/**
 * Nav item config.
 *
 * `systems` — if set, only show this item when currentSystem is in the list.
 * `systemModuleMap` — optional per-system NavModule override.
 *   e.g. { openclaw: 'openclaw' } means: when currentSystem is openclaw,
 *   open the 'openclaw' module instead of the default `key`.
 *   This allows the same nav label to open completely different pages per system.
 */
interface NavItem {
  key: NavModule
  labelKey: string
  icon: SvgComponent
  guiOnly?: boolean
  systems?: SystemOwner[]
  systemModuleMap?: Partial<Record<SystemOwner, NavModule>>
  /** Optional custom click action (bypasses navigateToModule). */
  action?: () => void
  /** Optional active-state override for items that reuse another module. */
  activeWhen?: () => boolean
}

const allTopNavItems: NavItem[] = [
  {
    key: 'assistant',
    labelKey: 'nav.assistant',
    icon: IconAssistant,
    // 同一导航在不同系统打开不同页面的配置方法
    systemModuleMap: {
      openclaw: 'openclaw', // OpenClaw 模式下打开 openclaw 模块
    },
  },
  {
    key: 'knowledge',
    labelKey: 'nav.knowledge',
    icon: IconKnowledge,
  },
  {
    key: 'scheduled-tasks',
    labelKey: 'nav.scheduledTasks',
    icon: IconCorn,
    systemModuleMap: {
      openclaw: 'openclaw-cron',
    },
  },
  {
    key: 'skills',
    labelKey: 'nav.skills',
    icon: IconSkills,
    systems: ['chatclaw'],
    systemModuleMap: {
      openclaw: 'openclaw-skills',
    },
  },
  {
    key: 'skillmarket',
    labelKey: 'nav.skillmarket',
    icon: IconSkills,
    systems: ['openclaw'],
  },
  {
    key: 'channels',
    labelKey: 'nav.channels',
    icon: IconChannels,
    systemModuleMap: {
      openclaw: 'openclaw-channels',
    },
  },
  {
    key: 'memory',
    labelKey: 'nav.memory',
    icon: IconMemory,
    systems: ['openclaw'],
  },
  {
    key: 'tools',
    labelKey: 'nav.tools',
    icon: IconTools,
  },
  {
    key: 'multiask',
    labelKey: 'nav.multiask',
    icon: IconMultiask,
    guiOnly: true,
  },
  {
    key: 'openclaw-runtime',
    labelKey: 'settings.menu.openclawRuntime',
    icon: IconSettings,
    guiOnly: true,
    systems: ['openclaw'],
  },
  {
    key: 'openclaw-terminal',
    labelKey: 'nav.openclawTerminal',
    icon: IconTerminal,
    guiOnly: true,
    systems: ['openclaw'],
  },
  // {
  //   key: 'openclaw-runtime-environment',
  //   labelKey: 'settings.menu.runtimeEnvironment',
  //   icon: IconSettings,
  //   guiOnly: true,
  //   systems: ['openclaw'],
  // },
]

const topNavItems = computed(() =>
  allTopNavItems.filter((item) => {
    if (item.guiOnly && !appStore.isGUIMode) return false
    if (item.systems && !item.systems.includes(appStore.currentSystem)) return false
    if (item.key === 'multiask' && !appStore.showMultiaskInNav) return false
    return true
  })
)

const bottomNavItems: NavItem[] = [
  {
    key: 'settings',
    labelKey: 'nav.settings',
    icon: IconSettings,
  },
]

/**
 * Resolve the effective NavModule for a nav item given the current system.
 */
const resolveModule = (item: NavItem): NavModule => {
  if (item.systemModuleMap) {
    const mapped = item.systemModuleMap[appStore.currentSystem]
    if (mapped) return mapped
  }
  return item.key
}

/**
 * Check if a nav item is active by comparing the resolved module.
 */
const isActive = (item: NavItem): boolean => {
  if (item.activeWhen) return item.activeWhen()
  const mod = resolveModule(item)
  return navigationStore.activeModule === mod
}

const openClawRuntimeRequiredAllowedModules = new Set<NavModule>([
  'openclaw-runtime',
  'settings',
  'tools',
  'knowledge',
  'multiask',
])

const shouldRedirectToRuntimeEnvironment = async (targetModule: NavModule): Promise<boolean> => {
  if (appStore.currentSystem !== 'openclaw') return false
  if (openClawRuntimeRequiredAllowedModules.has(targetModule)) return false
  try {
    const status = await ToolchainService.GetOpenClawRuntimeStatus()
    return !status?.installed
  } catch {
    // Treat unknown status as not installed to keep behavior safe and explicit.
    return true
  }
}

const handleNavClick = async (item: NavItem) => {
  if (item.action) {
    item.action()
    return
  }
  const mod = resolveModule(item)
  if (await shouldRedirectToRuntimeEnvironment(mod)) {
    navigationStore.navigateToModule('openclaw-runtime-environment', appStore.currentSystem)
    // Keep sidebar highlight on the item the user clicked; tab is the dedicated runtime gate page.
    navigationStore.activeModule = mod
    return
  }
  navigationStore.navigateToModule(mod, appStore.currentSystem)
}

/** Side nav row: OpenClaw active uses #FFE2E2 background; icon color applied separately. */
const navButtonClass = (item: NavItem) =>
  cn(
    'group mx-2 flex items-center gap-2 rounded-md px-2 py-[9px] text-left text-[15px] transition-colors',
    navigationStore.sidebarCollapsed && 'justify-center',
    isActive(item) ? 'font-bold' : 'font-normal',
    isActive(item)
      ? cn(
          'text-accent-foreground',
          appStore.currentSystem === 'openclaw' ? 'bg-[#FFE2E2]' : 'bg-accent'
        )
      : 'text-accent-foreground hover:bg-accent/50'
  )

const navIconClass = (item: NavItem) =>
  cn(
    'size-4 shrink-0 opacity-100',
    isActive(item) && appStore.currentSystem === 'openclaw' && 'text-[#DC2626]'
  )
</script>

<template>
  <div
    :class="
      cn(
        'flex shrink-0 flex-col items-center justify-between overflow-hidden border-r border-solid border-border bg-background py-3 transition-all duration-200',
        navigationStore.sidebarCollapsed ? 'w-13' : 'w-48'
      )
    "
  >
    <!-- Top navigation area -->
    <div class="flex w-full flex-col gap-1">
      <!-- System Switcher (Figma: pill 100px, #F5F5F5 border; hover #F0F0F0 + #D4D4D4 border) -->
      <div class="relative mx-2 mb-1">
        <TooltipProvider v-if="appStore.currentSystem === 'openclaw'" :delay-duration="300">
          <Tooltip>
            <TooltipTrigger as-child>
              <SideNavSystemSwitcherButton
                ref="switcherButtonCmp"
                :native-title="undefined"
                @click="toggleSwitcher"
              />
            </TooltipTrigger>
            <TooltipContent
              :show-arrow="false"
              side="right"
              align="center"
              :side-offset="8"
              :class="
                cn(
                  'z-[250] max-w-[min(280px,calc(100vw-32px))] border-0 bg-transparent p-0 text-foreground shadow-none'
                )
              "
            >
              <OpenClawGatewayStatusPillContent/>
            </TooltipContent>
          </Tooltip>
        </TooltipProvider>
        <SideNavSystemSwitcherButton
          v-else
          ref="switcherButtonCmp"
          :native-title="switcherNativeTitle"
          @click="toggleSwitcher"
        />
      </div>

      <!-- Dropdown teleported to body to escape SideNav's overflow-hidden / stacking-context constraints.
           Click-away is handled by a global mousedown listener (handleOutsideMouseDown) instead of a
           full-screen overlay, which prevents blocking Wails native drag-region events on the TitleBar. -->
      <Teleport to="body">
        <Transition
          enter-active-class="transition duration-150 ease-out"
          enter-from-class="scale-95 opacity-0"
          enter-to-class="scale-100 opacity-100"
          leave-active-class="transition duration-100 ease-in"
          leave-from-class="scale-100 opacity-100"
          leave-to-class="scale-95 opacity-0"
        >
          <div
            v-if="switcherOpen"
            ref="dropdownRef"
            class="fixed z-[200] flex flex-col gap-0.5 overflow-hidden rounded-md bg-popover p-0.5 shadow-[0_6px_30px_rgba(0,0,0,0.05),0_16px_24px_rgba(0,0,0,0.04),0_8px_10px_rgba(0,0,0,0.08)] dark:shadow-none dark:ring-1 dark:ring-white/10"
            :style="dropdownStyle"
          >
            <button
              v-for="opt in systemOptions"
              :key="opt.value"
              type="button"
              class="flex w-full min-w-0 items-center gap-2 rounded-md px-4 py-[8px] text-left text-[15px] font-bold leading-[22px] text-[#262626] transition-colors hover:bg-accent dark:text-popover-foreground"
              @click="selectSystem(opt.value)"
            >
              <span
                class="inline-flex size-5 shrink-0 items-center justify-center overflow-hidden rounded-[3.75px]"
              >
                <component
                  :is="opt.icon"
                  class="block size-full min-h-0 min-w-0 max-h-full max-w-full shrink-0"
                  preserveAspectRatio="xMidYMid meet"
                  aria-hidden="true"
                />
              </span>
              <span class="min-w-0 flex-1 truncate">{{ t(opt.labelKey) }}</span>
              <Check
                v-if="appStore.currentSystem === opt.value"
                class="size-4 shrink-0 text-foreground"
              />
            </button>
          </div>
        </Transition>
      </Teleport>

      <!-- Nav items -->
      <button
        v-for="item in topNavItems"
        :key="item.key"
        :class="navButtonClass(item)"
        :title="navigationStore.sidebarCollapsed ? t(item.labelKey) : undefined"
        @click="handleNavClick(item)"
      >
        <component :is="item.icon" width="16" height="16" :class="navIconClass(item)" />
        <span v-if="!navigationStore.sidebarCollapsed">{{ t(item.labelKey) }}</span>
      </button>
    </div>

    <!-- Bottom navigation area -->
    <div class="flex w-full flex-col gap-1">
      <ChatWikiSidebarAccountCard v-if="!navigationStore.sidebarCollapsed" />
      <button
        v-for="item in bottomNavItems"
        :key="item.key"
        :class="navButtonClass(item)"
        :title="navigationStore.sidebarCollapsed ? t(item.labelKey) : undefined"
        @click="handleNavClick(item)"
      >
        <component :is="item.icon" width="16" height="16" :class="navIconClass(item)" />
        <span v-if="!navigationStore.sidebarCollapsed">{{ t(item.labelKey) }}</span>
      </button>
    </div>
  </div>
</template>
