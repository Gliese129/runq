<template>
  <!-- RQ-74: VSCode-style bottom status bar — the dashboard's one honest
       line about "can I trust what I'm looking at": connection state,
       targets online, forward troubles, daemon identity/uptime. Turns
       amber in reconnect mode instead of letting the page die silently. -->
  <v-footer
    app
    height="26"
    class="status-bar px-3"
    :class="{ 'status-bar--warn': !conn.connected.value }"
  >
    <!-- connection segment -->
    <span class="seg" :title="conn.lastError.value">
      <span
        class="status-dot mr-1"
        :class="conn.connected.value ? 'status-dot--completed' : 'status-dot--failed sb-pulse'"
      />
      {{ conn.connected.value ? t('statusbar.connected') : t('statusbar.reconnecting') }}
    </span>

    <template v-if="health">
      <!-- targets online -->
      <span v-if="health.targets.length > 0" class="seg">
        {{ t('statusbar.targets', { n: reachableCount, total: health.targets.length }) }}
      </span>

      <!-- forward troubles only — a healthy forward earns no voice here -->
      <span v-for="fw in troubledForwards" :key="fw.name" class="seg seg--warn" :title="fw.detail">
        {{ fw.label }}
      </span>
    </template>

    <v-spacer />

    <!-- identity: whose daemon, which version, up how long -->
    <span v-if="health" class="seg seg--dim">
      {{ identityLabel }}
    </span>
  </v-footer>
</template>

<script setup lang="ts">
import { computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useQueryClient } from '@tanstack/vue-query'
import { useConnection } from '@/composables/useConnection'
import { useHealthQuery } from '@/queries/useHealthQuery'
import { formatDuration } from '@/utils/relativeTime'

const { t } = useI18n()
const conn = useConnection()
const qc = useQueryClient()

// The health poll doubles as the reconnect probe (3s while down).
const healthQuery = useHealthQuery()
const health = computed(() => healthQuery.data.value ?? null)

const reachableCount = computed(
  () => (health.value?.targets ?? []).filter((tg) => tg.reachable).length,
)

const troubledForwards = computed(() => {
  const fws = health.value?.forwards ?? {}
  return Object.entries(fws)
    .filter(([, fw]) => fw.state !== 'up')
    .map(([name, fw]) => ({
      name,
      label:
        fw.state === 'reconnecting'
          ? t('statusbar.forward_reconnecting', { name, attempts: fw.attempts ?? 0 })
          : t('statusbar.forward_stopped', { name }),
      detail: fw.last_error ?? '',
    }))
})

const identityLabel = computed(() => {
  const h = health.value
  if (!h) return ''
  const host = h.hostname ? `${h.hostname} · ` : ''
  return `${host}runq ${h.version} · ${t('statusbar.up', { dur: formatDuration(h.uptime_seconds) })}`
})

// Reconnect recovery (RQ-74): the moment the connection comes back, every
// cached query refetches — the page picks up where it left off instead of
// showing pre-restart data until the next poll.
watch(
  () => conn.connected.value,
  (now, before) => {
    if (now && before === false) void qc.invalidateQueries()
  },
)
</script>

<style scoped>
.status-bar {
  font-size: 11px;
  line-height: 1;
  border-top: 0.5px solid rgb(var(--v-theme-outline-variant));
  color: rgb(var(--v-theme-on-surface-variant));
  gap: 0;
  transition: background-color 0.3s ease;
}
.status-bar--warn {
  background: rgba(var(--v-theme-warning), 0.15);
}
.seg {
  display: inline-flex;
  align-items: center;
  padding: 0 10px;
  white-space: nowrap;
}
.seg + .seg {
  border-left: 0.5px solid rgb(var(--v-theme-outline-variant));
}
.seg--warn {
  color: rgb(var(--v-theme-warning));
}
.seg--dim {
  opacity: 0.75;
}
@keyframes sb-pulse {
  0%,
  100% {
    opacity: 1;
  }
  50% {
    opacity: 0.35;
  }
}
.sb-pulse {
  animation: sb-pulse 1.6s ease-in-out infinite;
}
@media (prefers-reduced-motion: reduce) {
  .sb-pulse {
    animation: none;
  }
}
</style>
