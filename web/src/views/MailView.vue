<script setup lang="ts">
// 邮件工作区（DESIGN.md 7.1/7.2）：左侧账号栏 + 右侧邮件流 + 详情抽屉。
// 已读与删除只作用于本地副本，绝不回写邮箱服务器。
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import {
  NInput, NButton, NSpace, NSwitch, NTag, NBadge, NDrawer, NDrawerContent, NEmpty,
  NSpin, NText, NTooltip, NSelect, NDatePicker, useMessage, useDialog,
} from 'naive-ui'
import { download, fmtSize, relTime, request, openEvents } from '../api'
import type { Account, Msg, MsgDetail } from '../types'

const message = useMessage()
const dialog = useDialog()

// ---- 账号栏 ----
const accountQ = ref('')
const accounts = ref<Account[]>([])
const totalAccounts = ref(0)
const selectedAccount = ref<number>(0) // 0 = 全部账号
let searchTimer: ReturnType<typeof setTimeout> | undefined

async function loadAccounts() {
  try {
    const q = accountQ.value.trim()
    const resp = await request<{ total: number; accounts: Account[] }>(
      'GET', `/api/v1/accounts?limit=200${q ? `&q=${encodeURIComponent(q)}` : ''}`)
    accounts.value = resp.accounts
    totalAccounts.value = resp.total
  } catch (e) { message.error((e as Error).message) }
}

const totalUnread = computed(() => accounts.value.reduce((s, a) => s + a.unread_count, 0))
const accountById = computed(() => {
  const m = new Map<number, Account>()
  accounts.value.forEach(a => m.set(a.id, a))
  return m
})

// ---- 邮件流 ----
const state = reactive({
  q: '', onlyUnread: false,
  loading: false, loadingMore: false,
  msgs: [] as Msg[], hasMore: false, total: 0,
})
const selected = ref<MsgDetail | null>(null)
const detailLoading = ref(false)
const showHtml = ref(false)
const loadImages = ref(false)
let msgTimer: ReturnType<typeof setTimeout> | undefined

// 标签筛选（按所属账号标签过滤邮件；选项来自 /tags）
const tagFilter = ref<string[]>([])
const tagOptions = ref<{ label: string; value: string }[]>([])
async function loadTagOptions() {
  try {
    const resp = await request<{ tags: { name: string; count: number }[] }>('GET', '/api/v1/tags')
    tagOptions.value = resp.tags.map(t => ({ label: `${t.name} (${t.count})`, value: t.name }))
  } catch { /* 静默降级为空选项 */ }
}
function pickTag(t: string) {
  tagFilter.value = tagFilter.value.includes(t) ? tagFilter.value : [...tagFilter.value, t]
  loadMessages()
}

// 时间范围筛选（[起始, 结束] 毫秒时间戳；后端 since/until，DESIGN.md 6.3）
const timeRange = ref<[number, number] | null>(null)

// ---- 列宽拖拽（发件人/收件人/时间可调；主题列 flex 占余宽）----
const WKEY = 'mailhub_mail_colw'
const colW = reactive({ acct: 170, from: 180, to: 200, time: 90 })
try { Object.assign(colW, JSON.parse(localStorage.getItem(WKEY) || '{}')) } catch { /* 隐私模式等 */ }
function saveColW() {
  try { localStorage.setItem(WKEY, JSON.stringify(colW)) } catch { /* 忽略 */ }
}
function startResize(key: 'acct' | 'from' | 'to' | 'time', e: MouseEvent) {
  const startX = e.clientX
  const startW = colW[key]
  const onMove = (ev: MouseEvent) => {
    colW[key] = Math.min(500, Math.max(60, startW + ev.clientX - startX))
  }
  const onUp = () => {
    document.removeEventListener('mousemove', onMove)
    document.removeEventListener('mouseup', onUp)
    document.body.style.cursor = ''
    saveColW()
  }
  document.body.style.cursor = 'col-resize'
  document.addEventListener('mousemove', onMove)
  document.addEventListener('mouseup', onUp)
  e.preventDefault()
  e.stopPropagation()
}

async function loadMessages(reset = true) {
  if (reset) {
    state.loading = true
    state.msgs = []
  } else {
    state.loadingMore = true
  }
  try {
    const p = new URLSearchParams()
    if (selectedAccount.value) p.set('account_id', String(selectedAccount.value))
    const q = state.q.trim()
    if (q) p.set('q', q)
    if (state.onlyUnread) p.set('is_read', 'false')
    tagFilter.value.forEach(t => p.append('tag', t))
    if (timeRange.value) {
      p.set('since', new Date(timeRange.value[0]).toISOString())
      p.set('until', new Date(timeRange.value[1]).toISOString())
    }
    p.set('limit', '50')
    if (sortKey.value !== 'default') {
      // 排序模式：后端按列排序 + offset 偏移分页（id 游标仅默认模式有效）
      p.set('sort', sortKey.value)
      p.set('order', sortOrder.value)
      if (!reset) p.set('offset', String(state.msgs.length))
    } else if (!reset && state.msgs.length) {
      p.set('before_id', String(state.msgs[state.msgs.length - 1].id))
    }
    if (!reset) p.set('no_total', '1') // 翻页跳过重复 COUNT 全表扫描，沿用首页 total
    const resp = await request<{ messages: Msg[]; total?: number }>('GET', `/api/v1/messages?${p}`)
    state.msgs = reset ? resp.messages : state.msgs.concat(resp.messages)
    if (resp.total !== undefined) state.total = resp.total
    state.hasMore = resp.messages.length > 0 && state.msgs.length < state.total
    if (reset && scrollEl.value) scrollEl.value.scrollTop = 0 // 换筛选回到顶部，避免深处误触发连加载
  } catch (e) { message.error((e as Error).message) }
  finally { state.loading = false; state.loadingMore = false }
}

// 无限滚动：距底 120px 自动加载下一页（与"加载更多"按钮并存，按钮作兜底）
const scrollEl = ref<HTMLElement | null>(null)
function onScroll(e: Event) {
  const el = e.target as HTMLElement
  if (!state.hasMore || state.loading || state.loadingMore) return
  if (el.scrollHeight - el.scrollTop - el.clientHeight < 120) loadMessages(false)
}

// ---- 排序（邮箱 / 时间；再次点击同列翻转方向）----
const sortKey = ref<'default' | 'time' | 'account'>('default')
const sortOrder = ref<'asc' | 'desc'>('desc')
function toggleSort(key: 'time' | 'account') {
  if (sortKey.value === key) {
    sortOrder.value = sortOrder.value === 'desc' ? 'asc' : 'desc'
  } else {
    sortKey.value = key
    sortOrder.value = key === 'account' ? 'asc' : 'desc' // 时间默认新→旧，邮箱默认 A→Z
  }
  loadMessages()
}
const sortArrow = (key: 'time' | 'account') =>
  sortKey.value === key ? (sortOrder.value === 'asc' ? '▲' : '▼') : ''

function watchAccountQ() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => { loadAccounts(); loadMessages() }, 300)
}
function watchMsgQ() {
  clearTimeout(msgTimer)
  msgTimer = setTimeout(() => loadMessages(), 300)
}

function pickAccount(id: number) {
  selectedAccount.value = id
  loadMessages()
}

// ---- 详情 ----
async function openDetail(m: Msg) {
  detailLoading.value = true
  selected.value = null
  showHtml.value = false
  loadImages.value = false
  try {
    const d = await request<MsgDetail>('GET', `/api/v1/messages/${m.id}`)
    selected.value = d
    // 打开详情默认标记已读（DESIGN.md 7.1）
    if (!d.is_read) toggleRead(d, true)
  } catch (e) { message.error((e as Error).message) }
  finally { detailLoading.value = false }
}

async function toggleRead(m: Msg, isRead: boolean) {
  try {
    await request('PATCH', `/api/v1/messages/${m.id}`, { is_read: isRead })
    m.is_read = isRead
    // 同步列表项与账号未读数（本地近似，等下次刷新校正）
    const inList = state.msgs.find(x => x.id === m.id)
    if (inList) inList.is_read = isRead
    const a = accountById.value.get(m.account_id)
    if (a && !isRead) a.unread_count++
    if (a && isRead && a.unread_count > 0) a.unread_count--
  } catch (e) { message.error((e as Error).message) }
}

function deleteMessage(m: Msg) {
  dialog.warning({
    title: '删除邮件',
    content: `确定删除「${m.subject || '(无主题)'}」？仅删除 MailHub 本地副本，不影响邮箱服务器。`,
    positiveText: '删除', negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await request('DELETE', `/api/v1/messages/${m.id}`)
        state.msgs = state.msgs.filter(x => x.id !== m.id)
        if (state.total > 0) state.total--
        selected.value = null
        message.success('已删除')
        loadAccounts()
      } catch (e) { message.error((e as Error).message) }
    },
  })
}

// ---- 批量操作 ----
const checked = ref<Set<number>>(new Set())
function toggleCheck(id: number) {
  if (checked.value.has(id)) checked.value.delete(id)
  else checked.value.add(id)
  checked.value = new Set(checked.value) // 触发响应

}
const allChecked = computed(() =>
  state.msgs.length > 0 && state.msgs.every(m => checked.value.has(m.id)))

function toggleAll() {
  if (allChecked.value) checked.value = new Set()
  else checked.value = new Set(state.msgs.map(m => m.id))
}

async function markAllRead() {
  try {
    const body = selectedAccount.value ? { account_id: selectedAccount.value } : { ids: [...checked.value] }
    if (!selectedAccount.value && checked.value.size === 0) {
      message.info('请先选择邮件，或进入某账号后标记该账号全部')
      return
    }
    const r = await request<{ updated: number }>('POST', '/api/v1/messages/batch-read', body)
    message.success(`已标记 ${r.updated} 封`)
    checked.value = new Set()
    loadMessages(); loadAccounts()
  } catch (e) { message.error((e as Error).message) }
}

function deleteChecked() {
  if (checked.value.size === 0) return
  dialog.warning({
    title: '批量删除',
    content: `确定删除选中的 ${checked.value.size} 封邮件？仅删除本地副本，不影响邮箱服务器。`,
    positiveText: '删除', negativeText: '取消',
    onPositiveClick: async () => {
      try {
        const r = await request<{ deleted: number }>('POST', '/api/v1/messages/batch-delete',
          { ids: [...checked.value] })
        message.success(`已删除 ${r.deleted} 封`)
        checked.value = new Set()
        selected.value = null
        loadMessages(); loadAccounts()
      } catch (e) { message.error((e as Error).message) }
    },
  })
}

function clearAccount() {
  const a = accountById.value.get(selectedAccount.value)
  if (!a) return
  dialog.warning({
    title: '清空账号邮件',
    content: `确定清空 ${a.email} 的全部 ${a.message_count} 封本地邮件？不影响邮箱服务器。`,
    positiveText: '清空', negativeText: '取消',
    onPositiveClick: async () => {
      try {
        const r = await request<{ deleted: number }>('POST', '/api/v1/messages/batch-delete',
          { account_id: selectedAccount.value })
        message.success(`已删除 ${r.deleted} 封`)
        loadMessages(); loadAccounts()
      } catch (e) { message.error((e as Error).message) }
    },
  })
}

async function syncAccount() {
  if (!selectedAccount.value) return
  try {
    await request('POST', `/api/v1/accounts/${selectedAccount.value}/sync`)
    message.success('已触发收取（后台执行）')
  } catch (e) { message.error((e as Error).message) }
}

// ---- 详情正文渲染 ----
// HTML 正文放入 sandbox iframe；默认不加载远程图片（防追踪像素，DESIGN.md 7.2）。
// 未允许加载图片时通过 CSP 彻底阻断一切外部网络请求（含无引号 src、srcset、CSS url() 等）。
const frameDoc = computed(() => {
  const d = selected.value
  if (!d || !d.body_html) return ''
  let html = d.body_html
  const csp = loadImages.value
    ? ''
    : `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:;">`
  if (!loadImages.value) {
    // 摘除图片源并展示占位提示，恢复时用原始 HTML 重建
    html = html.replace(/<img\b([^>]*)\bsrc\s*=\s*("([^"]*)"|'([^']*)'|[^\s>]+)/gi, '<img$1')
    html = html.replace(/<img\b([^>]*?)\/?>/gi,
      (m) => m.replace(/<img\b/i, '<img alt="[图片已屏蔽]"'))
  }
  return `<!doctype html><html><head><meta charset="utf-8">${csp}
<style>body{font-family:inherit;margin:8px;color:#333;word-break:break-word}img{max-width:100%}</style>
</head><body>${html}</body></html>`
})

function downloadRaw() {
  if (!selected.value) return
  download(`/api/v1/messages/${selected.value.id}/raw`, `${selected.value.id}.eml`)
}
function downloadAtt(attId: number, filename: string) {
  if (!selected.value) return
  download(`/api/v1/messages/${selected.value.id}/attachments/${attId}`, filename)
}

// ---- 生命周期 ----
// SSE：新邮件到达即时刷新（防抖 500ms 合并连发事件；断线由浏览器自动重连，
// 15s/30s 定时刷新仍保留作为兜底）
let es: { close: () => void } | null = null
let sseTimer: ReturnType<typeof setTimeout> | undefined
function onSSEMessage(data: string) {
  showNotify(data)
  clearTimeout(sseTimer)
  sseTimer = setTimeout(() => { loadMessages(); loadAccounts() }, 500)
}

// ---- 浏览器通知（M4）：新邮件到达且页面在后台时弹出 ----
// 仅本机/HTTPS 环境可用（Notification API 需 secure context）；权限须用户点开关时授予。
const canNotify = typeof Notification !== 'undefined'
const notifyOn = ref(canNotify
  && localStorage.getItem('mailhub_notify') === '1'
  && Notification.permission === 'granted')

async function toggleNotify(v: boolean) {
  if (!canNotify) {
    message.warning('当前环境不支持浏览器通知（需通过 localhost 或 HTTPS 访问）')
    return
  }
  if (v && Notification.permission !== 'granted') {
    const p = await Notification.requestPermission() // 须在用户手势内请求
    if (p !== 'granted') {
      message.error('通知权限被拒绝；可在浏览器地址栏站点设置中重新允许')
      return
    }
  }
  notifyOn.value = v
  try { localStorage.setItem('mailhub_notify', v ? '1' : '0') } catch { /* 隐私模式 */ }
}

function showNotify(data: string) {
  // 页面在前台时列表已即时刷新，不重复打扰；后台标签页才弹系统通知
  if (!notifyOn.value || document.visibilityState === 'visible') return
  let subject = '', from = '', account = ''
  try {
    const d = JSON.parse(data)
    subject = d.subject || ''; from = d.from || ''; account = d.account || ''
  } catch { /* 载荷异常：仍弹通用通知 */ }
  try {
    const n = new Notification(from || account || 'MailHub', {
      body: subject || '(无主题)',
      tag: 'mailhub-new', // 同 tag 覆盖，连发多封不堆积
    })
    n.onclick = () => { window.focus(); n.close() }
  } catch { /* 构造失败静默（列表刷新兜底） */ }
}

let refreshTimer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  loadAccounts()
  loadMessages()
  loadTagOptions()
  es = openEvents(onSSEMessage)
  refreshTimer = setInterval(() => { loadAccounts() }, 30000) // 心跳列定期刷新
})
onBeforeUnmount(() => {
  es?.close()
  clearInterval(refreshTimer)
  clearTimeout(searchTimer); clearTimeout(msgTimer); clearTimeout(sseTimer)
})
</script>

<template>
  <div class="mail-layout">
    <!-- 左侧账号栏 -->
    <aside class="account-panel">
      <div class="panel-header">
        <n-input v-model:value="accountQ" size="small" placeholder="搜索账号…" clearable
          @update:value="watchAccountQ" />
      </div>
      <div class="account-item" :class="{ active: selectedAccount === 0 }" @click="pickAccount(0)">
        <div class="account-row">
          <span class="account-name">全部账号</span>
          <n-badge v-if="totalUnread > 0" :value="totalUnread" :max="999" type="error" />
        </div>
        <div class="account-sub">{{ totalAccounts }} 个账号</div>
      </div>
      <div class="account-scroll">
        <div v-for="a in accounts" :key="a.id" class="account-item"
          :class="{ active: selectedAccount === a.id, warn: a.status === 'auth_failed' || a.status === 'error' }"
          @click="pickAccount(a.id)">
          <div class="account-row">
            <span class="account-name" :title="a.email">{{ a.name || a.email }}</span>
            <n-badge v-if="a.unread_count > 0" :value="a.unread_count" :max="999" type="error" />
          </div>
          <div class="account-sub">
            <span :class="{ stale: !a.last_sync_at }">心跳 {{ relTime(a.last_sync_at) }}</span>
            <n-tag v-if="a.status === 'auth_failed'" size="tiny" type="error">认证失败</n-tag>
            <n-tag v-else-if="a.status === 'error'" size="tiny" type="warning">错误</n-tag>
          </div>
        </div>
      </div>
    </aside>

    <!-- 右侧邮件流 -->
    <section class="mail-panel">
      <div class="toolbar">
        <n-input v-model:value="state.q" size="small" placeholder="搜索主题/发件人…" clearable
          style="width: 220px" @update:value="watchMsgQ" />
        <n-select v-model:value="tagFilter" size="small" multiple clearable
          :options="tagOptions" placeholder="按标签筛选" max-tag-count="responsive"
          style="width: 220px" @update:value="loadMessages()" />
        <n-date-picker v-model:value="timeRange" type="datetimerange" size="small" clearable
          :placeholder="['开始时间', '结束时间']" style="width: 330px" @update:value="loadMessages()" />
        <n-space align="center" :size="8">
          <n-text depth="3" size="small">仅未读</n-text>
          <n-switch v-model:value="state.onlyUnread" size="small" @update:value="loadMessages()" />
        </n-space>
        <n-space align="center" :size="8">
          <n-text depth="3" size="small">通知</n-text>
          <n-switch :value="notifyOn" size="small" @update:value="toggleNotify" />
        </n-space>
        <n-space :size="8" style="margin-left: auto">
          <n-button v-if="selectedAccount" size="small" @click="syncAccount">收取此账号</n-button>
          <n-button size="small" :disabled="!selectedAccount && checked.size === 0" @click="markAllRead">
            全部标已读
          </n-button>
          <n-button v-if="selectedAccount" size="small"
            :disabled="!accountById.get(selectedAccount)?.message_count" @click="clearAccount">
            清空此账号
          </n-button>
          <n-button size="small" type="error" secondary :disabled="checked.size === 0" @click="deleteChecked">
            删除选中 ({{ checked.size }})
          </n-button>
          <n-button size="small" quaternary @click="loadAccounts(); loadMessages()">刷新</n-button>
        </n-space>
      </div>

      <n-spin :show="state.loading">
        <div ref="scrollEl" class="msg-scroll" @scroll.passive="onScroll">
          <div v-if="state.msgs.length === 0 && !state.loading" class="empty-wrap">
            <n-empty description="暂无邮件（矩阵低频收件，新邮件到达后自动入库）" />
          </div>
          <!-- 表头行（邮箱/发件人/收件人/时间列宽可拖拽；邮箱与时间列头点击排序） -->
          <div v-if="state.msgs.length" class="msg-row msg-head">
            <input type="checkbox" :checked="allChecked" @change="toggleAll" />
            <span class="col-cell sortable" :style="{ width: colW.acct + 'px' }"
              title="按邮箱排序" @click="toggleSort('account')">邮箱{{ sortArrow('account') }}
              <span class="resize-h" title="拖拽调整列宽"
                @click.stop @mousedown="startResize('acct', $event)" /></span>
            <span class="col-cell" :style="{ width: colW.from + 'px' }">发件人
              <span class="resize-h" title="拖拽调整列宽" @mousedown="startResize('from', $event)" /></span>
            <span class="col-cell" :style="{ width: colW.to + 'px' }">收件人
              <span class="resize-h" title="拖拽调整列宽" @mousedown="startResize('to', $event)" /></span>
            <span class="col-cell sortable" :style="{ width: colW.time + 'px' }"
              title="按时间排序" @click="toggleSort('time')">时间{{ sortArrow('time') }}
              <span class="resize-h" title="拖拽调整列宽"
                @click.stop @mousedown="startResize('time', $event)" /></span>
            <span class="msg-subject">主题</span>
          </div>
          <div v-for="m in state.msgs" :key="m.id" class="msg-row"
            :class="{ unread: !m.is_read, selected: selected?.id === m.id }"
            @click="openDetail(m)">
            <input type="checkbox" :checked="checked.has(m.id)"
              @click.stop="toggleCheck(m.id)" />
            <div class="col-cell msg-acct" :style="{ width: colW.acct + 'px' }"
              :title="accountById.get(m.account_id)?.email || ''">
              {{ accountById.get(m.account_id)?.name
                 || accountById.get(m.account_id)?.email || ('#' + m.account_id) }}
            </div>
            <div class="col-cell msg-from" :style="{ width: colW.from + 'px' }">
              <div class="from-line" :title="m.from">{{ m.from || '—' }}</div>
              <div v-if="m.tags?.length" class="from-tags">
                <n-tag v-for="t in m.tags" :key="t" size="tiny" :bordered="false" type="info"
                  class="tag-chip" @click.stop="pickTag(t)">{{ t }}</n-tag>
              </div>
            </div>
            <div class="col-cell msg-to" :style="{ width: colW.to + 'px' }"
              :title="m.to?.join(', ')">{{ m.to?.length ? m.to.join(', ') : '—' }}</div>
            <div class="col-cell msg-time" :style="{ width: colW.time + 'px' }"
              :title="new Date(m.received_at).toLocaleString()">{{ relTime(m.received_at) }}</div>
            <span class="msg-subject">
              <template v-if="m.has_attachments">📎 </template>{{ m.subject || '(无主题)' }}
              <span class="snippet">{{ m.snippet }}</span>
            </span>
          </div>
          <div class="load-more">
            <template v-if="state.hasMore">
              <n-button size="small" quaternary :loading="state.loadingMore"
                @click="loadMessages(false)">加载更多</n-button>
              <n-text depth="3" size="small" style="margin-left: 8px">
                已加载 {{ state.msgs.length }} / 共 {{ state.total }} 封
              </n-text>
            </template>
            <n-text v-else-if="state.total > 0" depth="3" size="small">
              共 {{ state.total }} 封，已全部加载
            </n-text>
          </div>
        </div>
      </n-spin>
    </section>

    <!-- 详情抽屉 -->
    <n-drawer :show="!!selected" :width="520" placement="right"
      @update:show="(v: boolean) => { if (!v) selected = null }">
      <n-drawer-content :title="selected?.subject || '(无主题)'" closable>
        <n-spin :show="detailLoading">
          <div v-if="selected" class="detail">
            <div class="meta">
              <div><b>发件人</b>：{{ selected.from || '—' }}</div>
              <div v-if="selected.to?.length"><b>收件人</b>：{{ selected.to.join(', ') }}</div>
              <div><b>到达</b>：{{ new Date(selected.received_at).toLocaleString() }}
                <span v-if="selected.sent_at" class="meta-sub">
                  （发送 {{ new Date(selected.sent_at).toLocaleString() }}）
                </span>
              </div>
              <div><b>账号</b>：{{ accountById.get(selected.account_id)?.name
                || accountById.get(selected.account_id)?.email || selected.account_id }}
                · {{ selected.folder }}
                <n-tag v-for="t in selected.tags" :key="t" size="tiny" :bordered="false"
                  type="info" style="margin-left: 4px">{{ t }}</n-tag>
              </div>
              <div v-if="selected.parse_error">
                <n-tag type="warning" size="small">解析异常：{{ selected.parse_error }}</n-tag>
              </div>
            </div>
            <n-space size="small" style="margin: 12px 0">
              <n-button size="tiny" @click="toggleRead(selected, !selected.is_read)">
                {{ selected.is_read ? '标为未读' : '标为已读' }}
              </n-button>
              <n-button size="tiny" v-if="selected.body_html" @click="showHtml = !showHtml">
                {{ showHtml ? '纯文本' : 'HTML' }}
              </n-button>
              <n-button size="tiny" v-if="showHtml && !loadImages" @click="loadImages = true">
                加载远程图片
              </n-button>
              <n-button size="tiny" @click="downloadRaw">下载 .eml</n-button>
              <n-button size="tiny" type="error" secondary @click="deleteMessage(selected)">删除</n-button>
            </n-space>

            <!-- 正文 -->
            <iframe v-if="showHtml && selected.body_html" class="body-frame" sandbox=""
              :srcdoc="frameDoc" />
            <pre v-else class="body-text">{{ selected.body_text || '（无文本正文）' }}</pre>

            <!-- 附件 -->
            <div v-if="selected.attachments?.length" class="atts">
              <b>附件（{{ selected.attachments.length }}）</b>
              <div v-for="a in selected.attachments" :key="a.id" class="att-row">
                <span class="att-name" :title="a.filename">{{ a.filename }}</span>
                <span class="att-size">{{ fmtSize(a.size) }}</span>
                <n-button size="tiny" @click="downloadAtt(a.id, a.filename)">下载</n-button>
              </div>
            </div>
          </div>
        </n-spin>
      </n-drawer-content>
    </n-drawer>
  </div>
</template>

<style scoped>
.mail-layout { display: flex; height: 100%; }
.account-panel {
  width: 260px; border-right: 1px solid #e0e0e6; display: flex; flex-direction: column;
  background: #fafafa;
}
.panel-header { padding: 10px; border-bottom: 1px solid #e0e0e6; }
.account-scroll { flex: 1; overflow-y: auto; }
.account-item { padding: 8px 12px; cursor: pointer; border-bottom: 1px solid #f0f0f3; }
.account-item:hover { background: #f0f0f3; }
.account-item.active { background: #e8f2e8; }
.account-item.warn .account-sub { color: #d03050; }
.account-row { display: flex; align-items: center; justify-content: space-between; gap: 6px; }
.account-name { font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.account-sub {
  display: flex; gap: 6px; align-items: center; font-size: 11px; color: #999; margin-top: 2px;
}
.account-sub .stale { color: #d03050; }
.mail-panel { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.toolbar {
  display: flex; align-items: center; gap: 12px; padding: 10px 14px;
  border-bottom: 1px solid #e0e0e6; flex-wrap: wrap;
}
.msg-scroll { flex: 1; overflow-y: auto; min-height: 0; }
/* n-spin 生成的两层容器默认是普通 block，会截断 flex 链 → msg-scroll 高度变内容高、
   overflow 永不触发（表现为列表撑破视口被裁、滚不动）。显式传递 flex： */
.mail-panel :deep(.n-spin-container),
.mail-panel :deep(.n-spin-content) {
  flex: 1; min-height: 0; display: flex; flex-direction: column;
}
.empty-wrap { padding: 80px 0; }
.msg-row {
  display: flex; gap: 10px; padding: 9px 14px; border-bottom: 1px solid #f0f0f3;
  cursor: pointer; align-items: baseline;
}
.msg-row:hover { background: #f7f7fa; }
.msg-row.selected { background: #eef4ee; }
.msg-row.msg-head {
  cursor: default; font-size: 12px; color: #999; background: #fafafa;
  position: sticky; top: 0; z-index: 1;
}
.msg-row input[type="checkbox"] { cursor: pointer; align-self: center; }
/* 可调宽列：内容省略 + 右缘拖拽手柄（表头内） */
.col-cell {
  flex-shrink: 0; position: relative; overflow: hidden;
  font-size: 13px; white-space: nowrap; text-overflow: ellipsis;
}
.msg-to { color: #666; align-self: baseline; }
.msg-acct { color: #666; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sortable { cursor: pointer; user-select: none; }
.sortable:hover { color: #18a058; }
.resize-h {
  position: absolute; right: -3px; top: 0; width: 7px; height: 100%;
  cursor: col-resize; user-select: none;
}
.resize-h::after {
  content: ''; position: absolute; right: 3px; top: 20%; height: 60%; width: 1px;
  background: #d5d5dd;
}
.resize-h:hover::after { background: #18a058; width: 2px; }
.from-line { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.from-tags { display: flex; gap: 4px; margin-top: 2px; flex-wrap: wrap; }
.tag-chip { cursor: pointer; }
.msg-subject { flex: 1; min-width: 0; font-size: 13px; }
.msg-row.unread .msg-from, .msg-row.unread .msg-subject { font-weight: 700; }
.snippet { color: #aaa; font-weight: 400; font-size: 12px; margin-left: 8px; }
.msg-time { font-size: 12px; color: #999; }
.load-more { text-align: center; padding: 10px; }
.detail .meta { font-size: 13px; line-height: 1.8; color: #555; }
.meta-sub { color: #999; font-size: 12px; }
.body-frame { width: 100%; height: 480px; border: 1px solid #e0e0e6; border-radius: 4px; background: #fff; }
.body-text {
  white-space: pre-wrap; word-break: break-word; font-size: 13px; line-height: 1.7;
  background: #fafafa; padding: 12px; border-radius: 4px; max-height: 480px; overflow-y: auto;
  font-family: inherit; margin: 0;
}
.atts { margin-top: 16px; font-size: 13px; }
.att-row { display: flex; align-items: center; gap: 10px; padding: 6px 0; }
.att-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.att-size { color: #999; font-size: 12px; }
</style>
