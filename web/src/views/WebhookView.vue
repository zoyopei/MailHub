<script setup lang="ts">
// Webhook 管理工作区（DESIGN.md 7.4 / 6.4）：配置 CRUD、测试事件、投递记录与手动重发。
import { h, onMounted, ref } from 'vue'
import {
  NButton, NCard, NEmpty, NInput, NModal, NSelect, NSpace, NSwitch, NTag, NText,
  NDrawer, NDrawerContent, NDataTable, NPagination, NAlert, NPopconfirm,
  useMessage, useDialog, type DataTableColumns,
} from 'naive-ui'
import { relTime, request } from '../api'
import { fetchAllPages } from '../lib/maillist'
import type { Account } from '../types'

const message = useMessage()
const dialog = useDialog()

interface Webhook {
  id: number
  url: string
  account_ids: number[] | null
  include_body: boolean
  enabled: boolean
  created_at: string
  pending: number
  failed: number
}

interface DeliveryRow {
  id: number
  message_id: number
  status: string
  attempts: number
  next_retry_at: string | null
  response_code: number | null
  last_error: string | null
  updated_at: string
}

const webhooks = ref<Webhook[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const resp = await request<{ webhooks: Webhook[] }>('GET', '/api/v1/webhooks')
    webhooks.value = resp.webhooks
  } catch (e) { message.error((e as Error).message) }
  finally { loading.value = false }
}

// ---- 创建 / 编辑 ----
const showForm = ref(false)
const editingId = ref(0) // 0 = 创建
const form = ref({ url: '', account_ids: [] as number[] | null, include_body: false, enabled: true })
const saving = ref(false)
const createdSecret = ref('') // 创建成功后唯一一次显示

// 账号下拉选项：逐页取齐（一页最多 200 个）。只取第一页的话，矩阵超过 200 个账号时排在
// 后面的就选不进 Webhook 范围；下拉本身可搜索，账号多也找得到。
const accountOptions = ref<{ label: string; value: number }[]>([])

async function loadAccountOptions() {
  try {
    const resp = await fetchAllPages(page => request<{ total: number; accounts: Account[] }>(
      'GET', `/api/v1/accounts?limit=200&page=${page}`))
    accountOptions.value = resp.accounts.map(a => ({ label: a.email, value: a.id }))
  } catch { /* 下拉选项非关键 */ }
}

function openCreate() {
  editingId.value = 0
  form.value = { url: '', account_ids: null, include_body: false, enabled: true }
  createdSecret.value = ''
  showForm.value = true
}
function openEdit(w: Webhook) {
  editingId.value = w.id
  form.value = {
    url: w.url,
    account_ids: w.account_ids ?? null,
    include_body: w.include_body,
    enabled: w.enabled,
  }
  createdSecret.value = ''
  showForm.value = true
}

async function submitForm() {
  if (!form.value.url.trim()) return
  saving.value = true
  try {
    const body: Record<string, unknown> = {
      url: form.value.url.trim(),
      account_ids: form.value.account_ids ?? [],
      include_body: form.value.include_body,
      enabled: form.value.enabled,
    }
    if (editingId.value) {
      await request('PATCH', `/api/v1/webhooks/${editingId.value}`, body)
      message.success('已保存')
    } else {
      const resp = await request<{ secret: string }>('POST', '/api/v1/webhooks', body)
      createdSecret.value = resp.secret
      message.success('已创建')
    }
    load()
    if (!createdSecret.value) showForm.value = false // 创建保留弹窗展示 secret
  } catch (e) { message.error((e as Error).message) }
  finally { saving.value = false }
}

function removeWebhook(w: Webhook) {
  dialog.warning({
    title: '删除 Webhook',
    content: `确定删除 ${w.url}？投递记录将一并删除。`,
    positiveText: '删除', negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await request('DELETE', `/api/v1/webhooks/${w.id}`)
        message.success('已删除')
        load()
      } catch (e) { message.error((e as Error).message) }
    },
  })
}

async function toggleEnabled(w: Webhook, v: boolean) {
  try {
    await request('PATCH', `/api/v1/webhooks/${w.id}`, { enabled: v })
    w.enabled = v
  } catch (e) { message.error((e as Error).message) }
}

async function testWebhook(w: Webhook) {
  try {
    const r = await request<{ ok: boolean; status_code?: number; error?: string }>(
      'POST', `/api/v1/webhooks/${w.id}/test`)
    if (r.ok) message.success(`测试事件投递成功（HTTP ${r.status_code}）`)
    else message.error(`测试失败：${r.error}`)
  } catch (e) { message.error((e as Error).message) }
}

// ---- 投递记录 ----
const showDeliveries = ref(false)
const deliveries = ref<DeliveryRow[]>([])
const deliveryTotal = ref(0)
const deliveryPage = ref(1)
const deliveryStatus = ref<string | null>(null)
const deliveriesLoading = ref(false)
const currentHook = ref<Webhook | null>(null)

const statusOptions = [
  { label: '全部状态', value: '' },
  { label: 'pending（待投递）', value: 'pending' },
  { label: 'success（成功）', value: 'success' },
  { label: 'failed（失败）', value: 'failed' },
]

async function loadDeliveries() {
  if (!currentHook.value) return
  deliveriesLoading.value = true
  try {
    const p = new URLSearchParams()
    p.set('page', String(deliveryPage.value)); p.set('limit', '50')
    if (deliveryStatus.value) p.set('status', deliveryStatus.value)
    const r = await request<{ total: number; deliveries: DeliveryRow[] }>(
      'GET', `/api/v1/webhooks/${currentHook.value.id}/deliveries?${p}`)
    deliveries.value = r.deliveries
    deliveryTotal.value = r.total
  } catch (e) { message.error((e as Error).message) }
  finally { deliveriesLoading.value = false }
}

function openDeliveries(w: Webhook) {
  currentHook.value = w
  deliveryPage.value = 1
  deliveryStatus.value = null
  showDeliveries.value = true
  loadDeliveries()
}

async function retryDelivery(id: number) {
  try {
    await request('POST', `/api/v1/deliveries/${id}/retry`)
    message.success('已重新排队')
    loadDeliveries()
  } catch (e) { message.error((e as Error).message) }
}

function dStatusType(s: string): 'success' | 'error' | 'warning' {
  if (s === 'success') return 'success'
  if (s === 'failed') return 'error'
  return 'warning'
}

const deliveryColumns: DataTableColumns<DeliveryRow> = [
  { title: 'ID', key: 'id', width: 70 },
  { title: '状态', key: 'status', width: 90,
    render: r => h(NTag, { size: 'small', type: dStatusType(r.status) }, { default: () => r.status }) },
  { title: '尝试', key: 'attempts', width: 60 },
  { title: '邮件', key: 'message_id', width: 80 },
  { title: '响应码', key: 'response_code', width: 80,
    render: r => r.response_code == null ? '—' : String(r.response_code) },
  { title: '错误', key: 'last_error', ellipsis: { tooltip: true },
    render: r => r.last_error || '—' },
  { title: '更新时间', key: 'updated_at', width: 140, render: r => relTime(r.updated_at) },
  { title: '操作', key: 'op', width: 90, render: r =>
    h(NButton, { size: 'tiny', disabled: r.status === 'success', onClick: () => retryDelivery(r.id) },
      { default: () => '重发' }) },
]

onMounted(() => { load(); loadAccountOptions() })
</script>

<template>
  <div class="page">
    <div class="toolbar">
      <n-text depth="3">
        新邮件入库即投递（账号提醒开关 AND webhook 账号范围）；至少一次语义，下游以 delivery_id 幂等。
      </n-text>
      <n-space :size="8" style="margin-left: auto">
        <n-button size="small" @click="loadAccountOptions(); load()">刷新</n-button>
        <n-button size="small" type="primary" @click="openCreate">新建 Webhook</n-button>
      </n-space>
    </div>

    <div v-if="webhooks.length === 0 && !loading" class="empty-wrap">
      <n-empty description="暂无 Webhook。新建一个，把新邮件事件推送到你的系统。" />
    </div>

    <n-space vertical size="medium">
      <n-card v-for="w in webhooks" :key="w.id" size="small">
        <div class="hook-card">
          <div class="hook-main">
            <div class="hook-url">
              <n-tag :type="w.enabled ? 'success' : 'default'" size="small">
                {{ w.enabled ? '启用' : '停用' }}
              </n-tag>
              <span class="url-text" :title="w.url">{{ w.url }}</span>
              <n-tag v-if="w.include_body" size="small" type="info" :bordered="false">含正文</n-tag>
            </div>
            <div class="hook-meta">
              <span>账号范围：{{ w.account_ids === null ? '全部账号' : `${w.account_ids.length} 个指定账号` }}</span>
              <n-tag v-if="w.pending > 0" size="small" type="warning">待投递 {{ w.pending }}</n-tag>
              <n-tag v-if="w.failed > 0" size="small" type="error">失败 {{ w.failed }}</n-tag>
              <span v-else-if="w.pending === 0" class="ok">无积压</span>
            </div>
          </div>
          <n-space :size="6">
            <n-switch size="small" :value="w.enabled"
              @update:value="(v: boolean) => toggleEnabled(w, v)" />
            <n-button size="small" @click="testWebhook(w)">测试</n-button>
            <n-button size="small" @click="openDeliveries(w)">投递记录</n-button>
            <n-button size="small" @click="openEdit(w)">编辑</n-button>
            <n-button size="small" type="error" secondary @click="removeWebhook(w)">删除</n-button>
          </n-space>
        </div>
      </n-card>
    </n-space>

    <!-- 创建 / 编辑 -->
    <n-modal v-model:show="showForm" preset="card"
      :title="editingId ? '编辑 Webhook' : '新建 Webhook'" style="width: 560px">
      <n-space vertical size="medium">
        <n-alert v-if="createdSecret" type="warning" title="Secret 仅此一次显示">
          <code class="secret">{{ createdSecret }}</code>（请立即复制保存，用于校验签名）
        </n-alert>
        <n-input v-model:value="form.url" placeholder="https://your-service.example/webhook" />
        <n-space vertical size="small">
          <n-text depth="3" size="small">账号范围：不选 = 全部账号（仍受各账号"提醒开关"限制）</n-text>
          <n-select v-model:value="form.account_ids" multiple clearable filterable
            placeholder="全部账号" :options="accountOptions" :max-tag-count="5" />
        </n-space>
        <n-space align="center" :size="12">
          <span class="lbl">载荷附正文（include_body）</span>
          <n-switch v-model:value="form.include_body" size="small" />
          <span class="lbl">启用</span>
          <n-switch v-model:value="form.enabled" size="small" />
        </n-space>
        <n-space justify="end">
          <n-button v-if="createdSecret" @click="showForm = false">完成</n-button>
          <n-button v-else @click="showForm = false">取消</n-button>
          <n-button v-if="!createdSecret" type="primary" :loading="saving"
            :disabled="!form.url.trim()" @click="submitForm">
            {{ editingId ? '保存' : '创建' }}
          </n-button>
        </n-space>
      </n-space>
    </n-modal>

    <!-- 投递记录 -->
    <n-drawer v-model:show="showDeliveries" :width="860" placement="right">
      <n-drawer-content :title="`投递记录 — ${currentHook?.url || ''}`" closable>
        <n-space vertical size="medium">
          <n-space align="center" :size="12">
            <n-select v-model:value="deliveryStatus" size="small" :options="statusOptions"
              style="width: 180px" @update:value="deliveryPage = 1; loadDeliveries()" />
            <n-button size="small" quaternary @click="loadDeliveries">刷新</n-button>
          </n-space>
          <n-data-table :columns="deliveryColumns" :data="deliveries" size="small"
            :loading="deliveriesLoading" :max-height="520" :bordered="false" />
          <n-space justify="end">
            <n-pagination v-model:page="deliveryPage" :page-size="50" :item-count="deliveryTotal"
              @update:page="loadDeliveries()" />
          </n-space>
        </n-space>
      </n-drawer-content>
    </n-drawer>
  </div>
</template>

<style scoped>
.page { padding: 14px; height: 100%; overflow-y: auto; }
.toolbar { display: flex; align-items: center; gap: 12px; padding-bottom: 12px; flex-wrap: wrap; }
.empty-wrap { padding: 80px 0; }
.hook-card { display: flex; justify-content: space-between; align-items: center; gap: 16px; flex-wrap: wrap; }
.hook-main { min-width: 0; }
.hook-url { display: flex; align-items: center; gap: 8px; }
.url-text { font-weight: 600; word-break: break-all; }
.hook-meta { display: flex; gap: 10px; align-items: center; font-size: 12px; color: #888; margin-top: 6px; }
.hook-meta .ok { color: #18a058; }
.lbl { font-size: 13px; }
.secret { user-select: all; word-break: break-all; }
</style>
