<template>
  <!--
    Anime mode mascot placeholder.
    Each usage defines a pose via props — replace <img> src with actual assets later.
    Pose descriptions for image generation:

    mood="happy" variant="default"
      → Q版小人，开心表情，双手叉腰，自信站姿

    mood="happy" variant="sparkle"
      → Q版小人，开心表情，周围有星星闪烁，一只手举起打招呼

    mood="thinking" variant="sparkle"
      → Q版小人，歪头思考，手指托下巴，头顶有问号和星星

    mood="thinking" variant="default"
      → Q版小人，歪头思考，手指托下巴，一脸疑惑

    mood="sad" variant="default"
      → Q版小人，难过表情，低头垂手，头顶有乌云

    mood="happy" variant="gpu"
      → Q版小人，开心表情，双手举着一块GPU显卡，显卡发光
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
