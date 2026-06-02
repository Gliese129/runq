<template>
  <svg :width="size" :height="size" viewBox="0 0 36 36" xmlns="http://www.w3.org/2000/svg">
    <defs>
      <linearGradient id="logo-grad" x1="0%" y1="0%" x2="100%" y2="100%">
        <stop offset="0%" :stop-color="color1" />
        <stop offset="100%" :stop-color="color2" />
      </linearGradient>
    </defs>
    <rect width="36" height="36" rx="8" fill="url(#logo-grad)" />
    <!-- "r" letterform — minimal, geometric -->
    <path
      d="M11 25 L11 15 Q11 11 15 11 L19 11"
      stroke="white" stroke-width="2.8" fill="none" stroke-linecap="round" stroke-linejoin="round"
    />
    <!-- "q" letterform — circular with descender -->
    <circle cx="24" cy="18" r="5.5" stroke="white" stroke-width="2.5" fill="none" />
    <line x1="29.5" y1="20" x2="29.5" y2="27" stroke="white" stroke-width="2.5" stroke-linecap="round" />
  </svg>
</template>

<script setup lang="ts">
withDefaults(defineProps<{
  size?: number
}>(), {
  size: 36,
})

const color1 = 'rgb(var(--v-theme-primary))'
const color2 = 'rgb(var(--v-theme-secondary))'
</script>
