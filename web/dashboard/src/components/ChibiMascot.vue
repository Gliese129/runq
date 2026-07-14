<template>
  <!--
    Anime mode mascot placeholder.
    Each usage defines a pose via props — replace <img> src with actual assets later.
    Pose descriptions for image generation:

    mood="happy" variant="default"
      → Chibi character, smiling confidently with hands on hips

    mood="happy" variant="sparkle"
      → Chibi character, smiling and waving with stars sparkling around them

    mood="thinking" variant="sparkle"
      → Chibi character, head tilted in thought, finger on chin, with a question mark and stars overhead

    mood="thinking" variant="default"
      → Chibi character, head tilted in thought, finger on chin, looking puzzled

    mood="sad" variant="default"
      → Chibi character, looking sad with head and hands lowered, under a dark cloud

    mood="happy" variant="gpu"
      → Chibi character, smiling while holding a glowing GPU with both hands
  -->
  <div
    v-if="settings.animeMode"
    class="chibi-mascot d-inline-flex align-center justify-center"
    :style="{ width: size + 'px', height: size + 'px', opacity: 0.6 }"
  >
    <!-- Placeholder: replace with <img :src="..."> when assets are ready -->
    <v-icon :size="size * 0.5" color="primary" style="opacity: 0.4">mdi-account-heart-outline</v-icon>
  </div>
</template>

<script setup lang="ts">
import { useSettingsStore } from '@/stores/settings'

withDefaults(defineProps<{
  size?: number
  mood?: 'happy' | 'thinking' | 'sad'
  variant?: 'default' | 'gpu' | 'sparkle'
}>(), {
  size: 80,
  mood: 'happy',
  variant: 'default',
})

const settings = useSettingsStore()
</script>

<style scoped>
.chibi-mascot {
  transition: opacity 0.3s ease;
}
</style>
