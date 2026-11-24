<script setup lang="ts">
// 仪表盘（DESIGN.md 7.5）：账号状态分布、今日收信、投递积压、磁盘占用。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NCard, NGrid, NGi, NStatistic, NTag, NSpace, NText, NSpin, useMessage } from 'naive-ui'
import { fmtSize, request } from '../api'
import { localDayStart } from '../lib/daystart'
import type { StatusResp } from '../types'

const message = useMessage()
const loading = ref(true)
const stat = ref<StatusResp | null>(null)

const statusEntries = computed(() => {
  const m = stat.value?.accounts_by_status || {}
  return Object.entries(m).sort((a, b) => b[1] - a[1])
})
const totalAccounts = computed(() =>
  statusEntries.value.reduce((s, [, n]) => s + n, 0))

function statusType(s: string): 'success' | 'error' | 'warning' | 'info' | 'default' {
  switch (s) {
    case 'polling': case 'idle': return 'success'
    case 'auth_failed': return 'error'
    case 'error': return 'warning'
    case 'syncing': return 'info'
    default: return 'default'
  }
}

async function load() {
  try {
    // "今日"按浏览器所在时区算（day_start = 本地零点）。不传的话服务端按它自己的时区算，
    // 容器里通常是 UTC：东八区每天 00:00–08:00 显示的还是"昨天"的收信数。
    stat.value = await request<StatusResp>(
      'GET', `/api/v1/status?day_start=${encodeURIComponent(localDayStart())}`)
  } catch (e) { message.error((e as Error).message) }
  finally { loading.value = false }
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(load, 10000)
})
onBeforeUnmount(() => clearInterval(timer))
</script>

<template>
  <div class="page">
    <n-spin :show="loading">
      <n-space vertical size="large">
        <n-grid :cols="4" :x-gap="12" item-responsive responsive="screen">
          <n-gi span="4 s:2 m:1">
            <n-card size="small">
              <n-statistic label="账号总数" :value="totalAccounts" />
            </n-card>
          </n-gi>
          <n-gi span="4 s:2 m:1">
            <n-card size="small">
              <n-statistic label="今日收信" :value="stat?.messages_today ?? 0" />
            </n-card>
          </n-gi>
          <n-gi span="4 s:2 m:1">
            <n-card size="small">
              <n-statistic label="待投递 / 失败" :value="`${stat?.deliveries_pending ?? 0} / ${stat?.deliveries_failed ?? 0}`" />
            </n-card>
          </n-gi>
          <n-gi span="4 s:2 m:1">
            <n-card size="small">
              <n-statistic label="磁盘占用" :value="fmtSize(stat?.disk_usage_bytes ?? 0)" />
            </n-card>
          </n-gi>
        </n-grid>

        <n-card title="账号状态分布" size="small">
          <n-space v-if="statusEntries.length" size="large" item-style="display:flex;align-items:center;gap:6px">
            <n-space v-for="[s, n] in statusEntries" :key="s" :size="6"
              item-style="display:flex;align-items:center;gap:6px">
              <n-tag :type="statusType(s)" size="small">{{ s }}</n-tag>
              <n-text strong>{{ n }}</n-text>
            </n-space>
          </n-space>
          <n-text v-else depth="3">暂无账号</n-text>
        </n-card>

        <n-card size="small">
          <n-text depth="3">
            Webhook 投递已启用：新邮件入库即推送（账号提醒开关默认关，需在账号页开启）；
            投递失败自动重试 5 次（10s/1m/5m/30m/2h），已结束（成功或放弃）的投递记录保留 30 天。
          </n-text>
        </n-card>
      </n-space>
    </n-spin>
  </div>
</template>

<style scoped>
.page { padding: 14px; height: 100%; overflow-y: auto; }
</style>
