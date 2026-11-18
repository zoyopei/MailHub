<script setup lang="ts">
// 账号管理（DESIGN.md 7.3）：分页表格 + 状态/标签筛选 + 批量操作 + 批量导入 + 单个添加。
import { computed, h, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  NButton, NCard, NInput, NInputNumber, NSelect, NSpace, NSwitch, NTag, NDataTable,
  NPagination, NModal, NForm, NFormItem, NRadioGroup, NRadio, NText, NDynamicTags,
  NAlert, useMessage, useDialog,
  type DataTableColumns,
} from 'naive-ui'
import { relTime, request, postText } from '../api'
import type { Account } from '../types'

const message = useMessage()
const dialog = useDialog()

// ---- 列表 ----
const loading = ref(false)
const rows = ref<Account[]>([])
const total = ref(0)
const page = ref(1)
const limit = ref(50)
const q = ref('')
const statusFilter = ref<string | null>(null)
const tagFilter = ref<string[]>([])
let qTimer: ReturnType<typeof setTimeout> | undefined

const statusOptions = [
  { label: '全部状态', value: '' },
  { label: 'pending（待验证）', value: 'pending' },
  { label: 'polling（轮询中）', value: 'polling' },
  { label: 'idle（待机）', value: 'idle' },
  { label: 'syncing（同步中）', value: 'syncing' },
  { label: 'error（错误）', value: 'error' },
  { label: 'auth_failed（认证失败）', value: 'auth_failed' },
  { label: 'disabled（已停用）', value: 'disabled' },
]

// 标签选项（引用数降序），筛选与编辑共用
const tagOptions = ref<{ label: string; value: string }[]>([])
async function loadTags() {
  try {
    const resp = await request<{ tags: { name: string; count: number }[] }>('GET', '/api/v1/tags')
    tagOptions.value = resp.tags.map(t => ({ label: `${t.name} (${t.count})`, value: t.name }))
  } catch { /* 静默：筛选器降级为空选项 */ }
}

async function load() {
  loading.value = true
  try {
    const p = new URLSearchParams()
    p.set('page', String(page.value)); p.set('limit', String(limit.value))
    if (q.value.trim()) p.set('q', q.value.trim())
    if (statusFilter.value) p.set('status', statusFilter.value)
    tagFilter.value.forEach(t => p.append('tag', t))
    const resp = await request<{ total: number; accounts: Account[] }>(
      'GET', `/api/v1/accounts?${p}`)
    rows.value = resp.accounts
    total.value = resp.total
  } catch (e) { message.error((e as Error).message) }
  finally { loading.value = false }
}
function watchQ() {
  clearTimeout(qTimer)
  qTimer = setTimeout(() => { page.value = 1; load() }, 300)
}

function statusType(s: string): 'success' | 'error' | 'warning' | 'info' | 'default' {
  switch (s) {
    case 'polling': case 'idle': return 'success'
    case 'auth_failed': return 'error'
    case 'error': return 'warning'
    case 'syncing': return 'info'
    default: return 'default'
  }
}

// ---- 行操作 ----
async function toggleNotify(a: Account, v: boolean) {
  try {
    await request('PATCH', `/api/v1/accounts/${a.id}`, { notify_enabled: v })
    a.notify_enabled = v
  } catch (e) { message.error((e as Error).message) }
}
async function toggleEnabled(a: Account, v: boolean) {
  try {
    await request('PATCH', `/api/v1/accounts/${a.id}`, { enabled: v })
    a.enabled = v
    a.status = v ? 'pending' : 'disabled'
    load()
  } catch (e) { message.error((e as Error).message) }
}
async function syncOne(a: Account) {
  try {
    await request('POST', `/api/v1/accounts/${a.id}/sync`)
    message.success(`已触发 ${a.email} 收取`)
  } catch (e) { message.error((e as Error).message) }
}

// ---- 单账号设置（含标签；凭据留空 = 不修改，提交新凭据后自动重启 worker 重试） ----
const showCfg = ref(false)
const cfgAcct = ref<Account | null>(null)
const cfgForm = ref({
  name: '', poll_interval: 60, folders: [] as string[], tags: [] as string[],
  username: '', imap_host: '', password: '', client_id: '', refresh_token: '',
})
const savingCfg = ref(false)

function openCfg(a: Account) {
  cfgAcct.value = a
  cfgForm.value = {
    name: a.name || '', poll_interval: a.poll_interval || 60,
    folders: [...(a.folders || [])], tags: [...(a.tags || [])],
    username: a.username || '', imap_host: a.imap_host || '',
    password: '', client_id: a.client_id || '', refresh_token: '',
  }
  showCfg.value = true
}

async function saveCfg() {
  const a = cfgAcct.value
  if (!a) return
  const f = cfgForm.value
  if (!f.folders.length) { message.warning('收取文件夹不能为空'); return }
  // 只提交变化的字段；凭据仅非空时提交（不回显、不改不碰）
  const body: Record<string, unknown> = {}
  if (f.name !== (a.name || '')) body.name = f.name
  if (f.poll_interval !== a.poll_interval) body.poll_interval = f.poll_interval
  if (f.folders.join('|') !== (a.folders || []).join('|')) body.folders = f.folders
  if (f.tags.join('|') !== (a.tags || []).join('|')) body.tags = f.tags
  if (a.transport !== 'graph') {
    if (f.username !== (a.username || '')) body.username = f.username
    if (f.imap_host !== (a.imap_host || '')) body.imap_host = f.imap_host
  }
  if (a.auth_type === 'password') {
    if (f.password) body.password = f.password
  } else {
    if (f.client_id && f.client_id !== (a.client_id || '')) body.client_id = f.client_id
    if (f.refresh_token) body.refresh_token = f.refresh_token
  }
  if (Object.keys(body).length === 0) { message.info('没有修改'); return }
  savingCfg.value = true
  try {
    await request('PATCH', `/api/v1/accounts/${a.id}`, body)
    message.success('已保存')
    showCfg.value = false
    load()
    loadTags() // 引用数/孤儿回收后刷新选项
  } catch (e) { message.error((e as Error).message) }
  finally { savingCfg.value = false }
}

// ---- 重新获取令牌（授权码流：浏览器登录 → 粘贴跳转地址 → 兑换全新 refresh_token） ----
const reauthCode = ref('')
const reauthing = ref(false)
const reauthUrl = computed(() => {
  const a = cfgAcct.value
  if (!a || a.auth_type !== 'oauth2') return ''
  const cid = (cfgForm.value.client_id || a.client_id || '').trim()
  if (!cid) return ''
  const scope = a.transport === 'graph'
    ? 'https://graph.microsoft.com/.default offline_access'
    : 'https://outlook.office365.com/IMAP.AccessAsUser.All offline_access'
  const p = new URLSearchParams({
    client_id: cid, response_type: 'code', redirect_uri: redirectURI,
    scope, response_mode: 'query',
  })
  return `https://login.microsoftonline.com/common/oauth2/v2.0/authorize?${p}`
})
const redirectURI = 'https://login.microsoftonline.com/common/oauth2/nativeclient'
function openAuthPage() {
  if (reauthUrl.value) window.open(reauthUrl.value, '_blank')
}
// 复制授权链接（不在默认浏览器打开时：粘贴到任意浏览器/无痕窗口/其他设备）
async function copyAuthUrl() {
  const u = reauthUrl.value
  if (!u) return
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(u)
    } else { // 非 secure context（如局域网 IP 访问）回退
      const ta = document.createElement('textarea')
      ta.value = u
      ta.style.cssText = 'position:fixed;opacity:0'
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      ta.remove()
    }
    message.success('授权链接已复制')
  } catch { message.error('复制失败，请手动选中下方链接复制') }
}
async function reauthNow() {
  const a = cfgAcct.value
  if (!a || !reauthCode.value.trim()) return
  reauthing.value = true
  try {
    await request('POST', `/api/v1/accounts/${a.id}/reauth`, { code: reauthCode.value })
    message.success('已获取新令牌，收取任务已重启')
    reauthCode.value = ''
    showCfg.value = false
    load()
  } catch (e) { message.error((e as Error).message) }
  finally { reauthing.value = false }
}

function removeOne(a: Account) {
  dialog.warning({
    title: '删除账号',
    content: `确定删除 ${a.email}？邮件记录将级联删除。勾选"同时清理 .eml"请使用确认后的清理选项。`,
    positiveText: '删除', negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await request('DELETE', `/api/v1/accounts/${a.id}?purge=true`)
        message.success('已删除')
        load()
      } catch (e) { message.error((e as Error).message) }
    },
  })
}

// ---- 批量操作 ----
const checked = ref<Set<number>>(new Set())
function rowKey(r: Account) { return r.id }
function onCheck(keys: number[]) { checked.value = new Set(keys) }

async function batchPatch(body: Record<string, unknown>, okMsg: string) {
  if (checked.value.size === 0) return
  try {
    await request('PATCH', '/api/v1/accounts/batch', { ids: [...checked.value], ...body })
    message.success(okMsg)
    checked.value = new Set()
    load()
  } catch (e) { message.error((e as Error).message) }
}
function batchRemove() {
  if (checked.value.size === 0) return
  dialog.warning({
    title: '批量删除账号',
    content: `确定删除选中的 ${checked.value.size} 个账号及其全部本地邮件？`,
    positiveText: '删除', negativeText: '取消',
    onPositiveClick: async () => {
      for (const id of checked.value) {
        try { await request('DELETE', `/api/v1/accounts/${id}?purge=true`) } catch { /* 逐个尽力删 */ }
      }
      message.success('已删除')
      checked.value = new Set()
      load()
    },
  })
}
async function batchSync() {
  if (checked.value.size === 0) return
  try {
    await request('POST', '/api/v1/accounts/sync', { ids: [...checked.value] })
    message.success(`已排队收取 ${checked.value.size} 个账号（错峰执行）`)
  } catch (e) { message.error((e as Error).message) }
}

// ---- 表格列 ----
const columns = computed<DataTableColumns<Account>>(() => [
  { type: 'selection' },
  { title: '邮箱', key: 'email', minWidth: 220, ellipsis: { tooltip: true },
    render: (r) => h('span', { style: r.status === 'auth_failed' ? 'color:#d03050' : '' }, r.email) },
  { title: '名称', key: 'name', minWidth: 140, ellipsis: { tooltip: true },
    render: (r) => (r.name && r.name !== r.email)
      ? h('span', {}, r.name)
      : h(NText, { depth: 3, size: 'small' }, { default: () => '—' }) },
  { title: '状态', key: 'status', width: 110,
    render: (r) => h(NTag, { size: 'small', type: statusType(r.status) }, { default: () => r.status }) },
  { title: '传输', key: 'transport', width: 80,
    render: (r) => h(NTag, { size: 'small', bordered: false },
      { default: () => r.transport }) },
  { title: '标签', key: 'tags', width: 150,
    render: (r) => {
      const tags = r.tags || []
      if (tags.length === 0) return h(NText, { depth: 3, size: 'small' }, { default: () => '—' })
      return h('div', { style: 'display:flex;gap:4px;flex-wrap:wrap' },
        tags.map(t => h(NTag, {
          size: 'tiny', bordered: false, type: 'info',
          style: 'cursor:pointer',
          onClick: () => { tagFilter.value = [t]; page.value = 1; load() },
        }, { default: () => t })))
    } },
  { title: '心跳', key: 'last_sync_at', width: 110,
    render: (r) => relTime(r.last_sync_at) },
  { title: '最新邮件', key: 'last_message_at', width: 110,
    render: (r) => relTime(r.last_message_at) },
  { title: '邮件/未读', key: 'message_count', width: 100,
    render: (r) => `${r.message_count} / ${r.unread_count}` },
  { title: '提醒', key: 'notify_enabled', width: 70,
    render: (r) => h(NSwitch, { size: 'small', value: r.notify_enabled,
      'onUpdate:value': (v: boolean) => toggleNotify(r, v) }) },
  { title: '启用', key: 'enabled', width: 70,
    render: (r) => h(NSwitch, { size: 'small', value: r.enabled,
      'onUpdate:value': (v: boolean) => toggleEnabled(r, v) }) },
  { title: '操作', key: 'actions', width: 160, render: (r) => h(NSpace, { size: 4 }, {
    default: () => [
      h(NButton, { size: 'tiny', onClick: () => syncOne(r) }, { default: () => '收取' }),
      h(NButton, { size: 'tiny', secondary: true, onClick: () => openCfg(r) },
        { default: () => '设置' }),
      h(NButton, { size: 'tiny', type: 'error', secondary: true, onClick: () => removeOne(r) },
        { default: () => '删除' }),
    ],
  }) },
])

// ---- 批量导入 ----
const showImport = ref(false)
const importText = ref('')
const importing = ref(false)
const importResult = ref<{ created: number; failed: { line: number; error: string }[] | null } | null>(null)

function openImport() {
  showImport.value = true
  importResult.value = null
}
function onImportFile(ev: Event) {
  const f = (ev.target as HTMLInputElement).files?.[0]
  if (!f) return
  const reader = new FileReader()
  reader.onload = () => { importText.value = String(reader.result || '') }
  reader.readAsText(f)
}
async function submitImport() {
  if (!importText.value.trim()) return
  importing.value = true
  importResult.value = null
  try {
    importResult.value = await postText('/api/v1/accounts/batch', importText.value) as never
    load()
  } catch (e) { message.error((e as Error).message) }
  finally { importing.value = false }
}

// ---- 单个添加 ----
const showAdd = ref(false)
const addForm = ref({
  email: '', name: '', auth_type: 'password', password: '',
  client_id: '', refresh_token: '', imap_host: '', tags: [] as string[],
})
const adding = ref(false)
async function submitAdd() {
  adding.value = true
  try {
    const body: Record<string, unknown> = {
      email: addForm.value.email, name: addForm.value.name, tags: addForm.value.tags,
    }
    if (addForm.value.imap_host) body.imap_host = addForm.value.imap_host
    if (addForm.value.auth_type === 'password') body.password = addForm.value.password
    else {
      body.client_id = addForm.value.client_id
      body.refresh_token = addForm.value.refresh_token
    }
    await request('POST', '/api/v1/accounts', body)
    message.success('已添加（登录验证通过）')
    showAdd.value = false
    Object.assign(addForm.value, { email: '', name: '', password: '', client_id: '', refresh_token: '', imap_host: '', tags: [] })
    load()
    loadTags()
  } catch (e) { message.error((e as Error).message) }
  finally { adding.value = false }
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  loadTags()
  timer = setInterval(load, 15000) // 状态/心跳定期刷新
})
onBeforeUnmount(() => {
  clearInterval(timer)
  clearTimeout(qTimer)
})
</script>

<template>
  <div class="page">
    <div class="toolbar">
      <n-input v-model:value="q" size="small" placeholder="搜索名称/邮箱…" clearable style="width: 220px"
        @update:value="watchQ" />
      <n-select v-model:value="statusFilter" size="small" :options="statusOptions"
        style="width: 190px" @update:value="page = 1; load()" />
      <n-select v-model:value="tagFilter" size="small" multiple clearable
        :options="tagOptions" placeholder="按标签筛选" max-tag-count="responsive"
        style="width: 220px" @update:value="page = 1; load()" />
      <n-space :size="8" style="margin-left: auto">
        <n-button size="small" type="primary" @click="openImport">批量导入</n-button>
        <n-button size="small" @click="showAdd = true">添加账号</n-button>
        <n-button size="small" :disabled="checked.size === 0" @click="batchSync">
          收取选中 ({{ checked.size }})
        </n-button>
        <n-button size="small" :disabled="checked.size === 0" @click="batchPatch({ enabled: true }, '已启用')">
          启用
        </n-button>
        <n-button size="small" :disabled="checked.size === 0" @click="batchPatch({ enabled: false }, '已停用')">
          停用
        </n-button>
        <n-button size="small" :disabled="checked.size === 0" @click="batchPatch({ notify_enabled: true }, '已开启提醒')">
          提醒开
        </n-button>
        <n-button size="small" :disabled="checked.size === 0" @click="batchPatch({ notify_enabled: false }, '已关闭提醒')">
          提醒关
        </n-button>
        <n-button size="small" type="error" secondary :disabled="checked.size === 0" @click="batchRemove">
          删除
        </n-button>
      </n-space>
    </div>

    <n-data-table v-model:checked-row-keys="checked" as="checkedRowKeys" :columns="columns" :data="rows"
      :row-key="rowKey" :loading="loading" :bordered="false" size="small"
      :max-height="`calc(100vh - 220px)`" :scroll-x="1400"
      @update:checked-row-keys="(keys: number[]) => onCheck(keys as number[])" />

    <div class="pager">
      <n-text depth="3" size="small">共 {{ total }} 个账号</n-text>
      <n-pagination v-model:page="page" :page-size="limit" :item-count="total"
        @update:page="load()" />
    </div>

    <!-- 批量导入对话框 -->
    <n-modal v-model:show="showImport" preset="card" title="批量导入 Outlook/Hotmail"
      style="width: 640px">
      <n-space vertical size="medium">
        <n-text depth="3">
          每行 <code>邮箱----密码----client_id----令牌</code>（与 main.py 账号文件逐字兼容；
          密码段不使用、不存储；Outlook/Hotmail 域名自动走 Graph 传输）。支持 <code>#</code> 注释与空行。
        </n-text>
        <div>
          <input type="file" accept=".txt" @change="onImportFile" />
        </div>
        <n-input v-model:value="importText" type="textarea" :rows="10" mono
          placeholder="a1@outlook.com----unused----00000000-…----O.XXXX…" />
        <n-space justify="end">
          <n-button @click="showImport = false">取消</n-button>
          <n-button type="primary" :loading="importing" :disabled="!importText.trim()"
            @click="submitImport">导入</n-button>
        </n-space>
        <n-card v-if="importResult" size="small" :title="`导入完成：成功 ${importResult.created} 个`">
          <template v-if="importResult.failed?.length">
            <div v-for="f in importResult.failed" :key="f.line" class="fail-line">
              第 {{ f.line }} 行：{{ f.error }}
            </div>
          </template>
          <n-text v-else depth="3">无失败行</n-text>
        </n-card>
      </n-space>
    </n-modal>

    <!-- 添加账号对话框 -->
    <n-modal v-model:show="showAdd" preset="card" title="添加账号" style="width: 520px">
      <n-form label-placement="left" label-width="100">
        <n-form-item label="邮箱" required>
          <n-input v-model:value="addForm.email" placeholder="me@example.com" />
        </n-form-item>
        <n-form-item label="名称">
          <n-input v-model:value="addForm.name" placeholder="可选，默认取邮箱" />
        </n-form-item>
        <n-form-item label="认证方式">
          <n-radio-group v-model:value="addForm.auth_type">
            <n-radio value="password">密码/授权码</n-radio>
            <n-radio value="oauth2">OAuth2（Outlook）</n-radio>
          </n-radio-group>
        </n-form-item>
        <n-form-item v-if="addForm.auth_type === 'password'" label="密码" required>
          <n-input v-model:value="addForm.password" type="password" show-password-on="click" />
        </n-form-item>
        <template v-else>
          <n-form-item label="client_id" required>
            <n-input v-model:value="addForm.client_id" placeholder="Azure 应用注册 ID" />
          </n-form-item>
          <n-form-item label="refresh_token" required>
            <n-input v-model:value="addForm.refresh_token" type="password" show-password-on="click" />
          </n-form-item>
        </template>
        <n-form-item label="IMAP 主机">
          <n-input v-model:value="addForm.imap_host"
            placeholder="留空按域名预设（Outlook 家族自动 Graph 传输）" />
        </n-form-item>
        <n-form-item label="标签">
          <n-dynamic-tags v-model:value="addForm.tags" />
        </n-form-item>
        <n-space justify="end">
          <n-button @click="showAdd = false">取消</n-button>
          <n-button type="primary" :loading="adding" :disabled="!addForm.email" @click="submitAdd">
            添加（先验证登录）
          </n-button>
        </n-space>
      </n-form>
    </n-modal>
    <!-- 单账号设置对话框 -->
    <n-modal v-model:show="showCfg" preset="card" :title="`账号设置 — ${cfgAcct?.email || ''}`"
      style="width: 560px">
      <n-space vertical size="medium">
        <n-alert v-if="cfgAcct?.status === 'auth_failed'" type="error" :show-icon="true">
          认证失败：{{ cfgAcct?.last_error || '凭据无效' }}。可在下方"重新获取令牌"走授权流程，
          或粘贴新的 refresh_token 后保存，均会自动重启收取。
        </n-alert>

        <n-form label-placement="left" label-width="110" size="small">
          <n-form-item label="名称">
            <n-input v-model:value="cfgForm.name" placeholder="默认取邮箱" />
          </n-form-item>
          <n-form-item label="轮询间隔（秒）">
            <n-input-number v-model:value="cfgForm.poll_interval" :min="5" :max="86400"
              :style="{ width: '160px' }" />
            <n-text depth="3" size="small" style="margin-left: 10px">
              低频邮箱建议 300–600
            </n-text>
          </n-form-item>
          <n-form-item label="收取文件夹">
            <n-dynamic-tags v-model:value="cfgForm.folders" />
          </n-form-item>
          <n-text depth="3" size="small" style="margin: -8px 0 4px 110px; display: block">
            默认 INBOX；IMAP 账号配置多个文件夹时只轮询不走 IDLE
          </n-text>
          <n-form-item label="标签">
            <n-dynamic-tags v-model:value="cfgForm.tags" />
          </n-form-item>

          <template v-if="cfgAcct?.transport !== 'graph'">
            <n-form-item label="用户名">
              <n-input v-model:value="cfgForm.username" placeholder="默认取邮箱地址" />
            </n-form-item>
            <n-form-item label="IMAP 主机">
              <n-input v-model:value="cfgForm.imap_host" placeholder="如 imap.example.com" />
            </n-form-item>
          </template>

          <n-form-item v-if="cfgAcct?.auth_type === 'password'" label="密码">
            <n-input v-model:value="cfgForm.password" type="password" show-password-on="click"
              placeholder="留空 = 不修改" />
          </n-form-item>
          <template v-else>
            <n-form-item label="client_id">
              <n-input v-model:value="cfgForm.client_id" placeholder="Azure 应用注册 ID" />
            </n-form-item>
            <n-form-item label="refresh_token">
              <n-input v-model:value="cfgForm.refresh_token" type="password"
                show-password-on="click" placeholder="留空 = 不修改" />
            </n-form-item>
            <n-form-item label="重新获取令牌">
              <n-space vertical size="small" style="width: 100%">
                <n-space align="center" :size="8">
                  <n-button size="small" type="primary" secondary :disabled="!reauthUrl"
                    @click="openAuthPage">
                    1. 打开 Microsoft 授权页
                  </n-button>
                  <n-button size="small" quaternary :disabled="!reauthUrl" @click="copyAuthUrl">
                    复制链接
                  </n-button>
                  <n-text depth="3" size="small">登录该邮箱账号并同意授权</n-text>
                </n-space>
                <n-text v-if="reauthUrl" depth="3" size="small" class="auth-url"
                  :title="reauthUrl" @click="copyAuthUrl">{{ reauthUrl }}</n-text>
                <n-input v-model:value="reauthCode" type="textarea" :rows="2"
                  placeholder="2. 授权完成后浏览器会跳转，把跳转后的完整地址（含 code=…）粘贴到这里" />
                <n-space align="center" :size="8">
                  <n-button size="small" type="warning" secondary :loading="reauthing"
                    :disabled="!reauthCode.trim()" @click="reauthNow">
                    3. 兑换新令牌
                  </n-button>
                  <n-text depth="3" size="small">
                    兑换成功后立即用新令牌重启收取；授权码一次性、约 10 分钟内有效。
                  </n-text>
                </n-space>
              </n-space>
            </n-form-item>
          </template>
        </n-form>

        <n-text depth="3" size="small">
          连接、间隔、文件夹或凭据的变更保存后会自动重启该账号的收取任务（错峰启动）。
        </n-text>
        <n-space justify="end">
          <n-button @click="showCfg = false">取消</n-button>
          <n-button type="primary" :loading="savingCfg" @click="saveCfg">保存</n-button>
        </n-space>
      </n-space>
    </n-modal>
  </div>
</template>

<style scoped>
.page { padding: 14px; height: 100%; display: flex; flex-direction: column; overflow: hidden; }
.toolbar {
  display: flex; gap: 12px; padding-bottom: 12px; align-items: center; flex-wrap: wrap;
}
.pager { display: flex; align-items: center; justify-content: space-between; padding-top: 10px; }
.fail-line { font-size: 12px; color: #d03050; padding: 2px 0; }
.auth-url {
  display: block; word-break: break-all; user-select: all; cursor: pointer;
  max-height: 3.6em; overflow: hidden; line-height: 1.2; margin-top: 2px;
}
</style>
