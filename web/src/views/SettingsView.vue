<script setup lang="ts">
// 全局配置页（M4）：运行时可变项在线修改，优先级 DB 覆盖 > 环境变量 > 代码默认。
// 凭据（API Token / SECRET_KEY）与进程级项（监听地址 / 数据目录）不在此——修改需重启。
import { onMounted, reactive, ref } from 'vue'
import {
  NCard, NForm, NFormItem, NInputNumber, NButton, NSpace, NTag, NAlert, NText, NSpin,
  useMessage,
} from 'naive-ui'
import { request } from '../api'
import type { SettingsResp } from '../types'

interface FieldDef {
  key: string
  label: string
  min: number
  max: number
  zeroText?: string // 0 的特殊含义
  hint: string      // 生效时机说明
}

const FIELDS: FieldDef[] = [
  { key: 'retention_days', label: '邮件保留天数', min: 0, max: 36500,
    zeroText: '0 = 永久保留', hint: '早于该窗口的邮件（含 .eml 文件）在每日维护任务中清理' },
  { key: 'initial_sync_days', label: '首次同步窗口（天）', min: 1, max: 3650,
    hint: '新添加账号只拉取最近 N 天的历史邮件且不触发通知，避免通知轰炸' },
  { key: 'poll_interval_default', label: '新账号默认轮询间隔（秒）', min: 5, max: 86400,
    hint: '对之后新增/导入的账号生效；已有账号的间隔在账号设置中单独调整' },
  { key: 'fallback_interval', label: 'IDLE 兜底拉取间隔（秒）', min: 30, max: 86400,
    hint: 'IDLE 账号周期性主动拉取的间隔（等待无通知时的兜底），下一轮等待周期生效' },
  { key: 'idle_budget_per_host', label: '单 IMAP 主机 IDLE 预算', min: 0, max: 100000,
    zeroText: '0 = 不限', hint: '超预算账号自动降级轮询；对新建立的 IDLE 会话生效' },
]

const message = useMessage()
const loading = ref(true)
const saving = ref(false)
const form = reactive<Record<string, number>>({})
const overridden = reactive<Record<string, boolean>>({})
const initial = reactive<Record<string, number>>({})

async function load() {
  loading.value = true
  try {
    const resp = await request<SettingsResp>('GET', '/api/v1/settings')
    for (const f of FIELDS) {
      const it = resp.settings[f.key]
      form[f.key] = it.value
      overridden[f.key] = it.overridden
      initial[f.key] = it.value
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 只提交实际修改的项：未动过的项保持"未覆盖"状态（不把环境变量层的值写脏成 DB 覆盖）
function dirtyBody(): Record<string, number | null> {
  const out: Record<string, number | null> = {}
  for (const f of FIELDS) {
    if (form[f.key] !== initial[f.key]) out[f.key] = form[f.key]
  }
  return out
}

async function save() {
  const body = dirtyBody()
  if (Object.keys(body).length === 0) {
    message.info('没有修改')
    return
  }
  saving.value = true
  try {
    await request('PUT', '/api/v1/settings', body)
    message.success('已保存')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

// 清除单项覆盖：回退到环境变量/代码默认层
async function resetOne(key: string) {
  try {
    await request('PUT', '/api/v1/settings', { [key]: null })
    message.success('已恢复（跟随环境变量）')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}
</script>

<template>
  <div class="settings-page">
    <n-spin :show="loading">
      <n-space vertical size="large">
        <n-alert type="info" :show-icon="true">
          在线修改的全局配置保存在数据库中，优先级：<n-text code>数据库覆盖 &gt; 环境变量 &gt; 默认值</n-text>。
          凭据（API Token / SECRET_KEY）与进程级配置（监听地址 / 数据目录）需修改环境变量后重启生效。
        </n-alert>

        <n-card title="同步与维护">
          <n-form label-placement="left" label-width="220" class="settings-form">
            <n-form-item v-for="f in FIELDS" :key="f.key">
              <template #label>
                <span class="flabel">
                  {{ f.label }}
                  <n-tag v-if="overridden[f.key]" size="small" type="warning" :bordered="false">
                    已覆盖
                  </n-tag>
                </span>
              </template>
              <div class="frow">
                <n-input-number
                  v-model:value="form[f.key]" :min="f.min" :max="f.max"
                  :style="{ width: '180px' }" button-placement="both"
                />
                <n-button
                  v-if="overridden[f.key]" quaternary size="small" type="warning"
                  @click="resetOne(f.key)"
                >
                  恢复默认
                </n-button>
                <div class="hint">
                  {{ f.hint }}<template v-if="f.zeroText">（{{ f.zeroText }}）</template>
                </div>
              </div>
            </n-form-item>
          </n-form>
          <template #footer>
            <n-space align="center" justify="space-between" style="width: 100%">
              <n-text depth="3" style="font-size: 12px">
                修改立即保存到数据库；各项生效时机见说明文字
              </n-text>
              <n-button type="primary" :loading="saving" @click="save">保存修改</n-button>
            </n-space>
          </template>
        </n-card>
      </n-space>
    </n-spin>
  </div>
</template>

<style scoped>
.settings-page {
  height: 100%;
  overflow-y: auto;
  padding: 20px 24px;
  background: #f3f5f7;
}
.settings-page :deep(.n-card) { max-width: 860px; }
.flabel { display: inline-flex; align-items: center; gap: 6px; }
.frow { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; flex: 1; }
.hint { font-size: 12px; color: #909399; flex: 1; min-width: 240px; }
</style>
