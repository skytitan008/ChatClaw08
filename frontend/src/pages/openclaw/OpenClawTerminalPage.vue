<script setup lang="ts">
import OpenClawTerminal from '@/components/terminal/OpenClawTerminal.vue'
</script>

<template>
  <div class="flex h-full w-full bg-background text-foreground">
    <main class="flex flex-1 flex-col overflow-hidden">
      <div class="h-full w-full">
        <OpenClawTerminal />
      </div>
    </main>
  </div>
</template>
