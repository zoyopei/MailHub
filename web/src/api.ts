// fetch 封装：统一 Bearer 认证、错误解析、带认证的文件下载。
const TOKEN_KEY = 'mailhub_token'

export let token = localStorage.getItem(TOKEN_KEY) || ''

export function setToken(t: string) {
  token = t
  localStorage.setItem(TOKEN_KEY, t)
}

export function clearToken() {
  token = ''
  localStorage.removeItem(TOKEN_KEY)
}

export class ApiError extends Error {
  status: number
  constructor(msg: string, status: number) {
    super(msg)
    this.status = status
  }
}

export async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (token) headers['Authorization'] = `Bearer ${token}`
  const resp = await fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (resp.status === 401) {
    clearToken()
    window.dispatchEvent(new Event('mailhub-unauthorized'))
    throw new ApiError('未授权：请重新输入 API Token', 401)
  }
  if (!resp.ok) {
    let msg = `HTTP ${resp.status}`
    try {
      const j = await resp.json()
      msg = j?.error?.message || msg
    } catch { /* 非 JSON 错误体 */ }
    throw new ApiError(msg, resp.status)
  }
  return resp.json() as Promise<T>
}

// 发送 text/plain（批量导入 ---- 格式文本）
export async function postText(path: string, text: string): Promise<unknown> {
  const resp = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'text/plain', Authorization: `Bearer ${token}` },
    body: text,
  })
  if (resp.status === 401) {
    clearToken()
    window.dispatchEvent(new Event('mailhub-unauthorized'))
    throw new ApiError('未授权：请重新输入 API Token', 401)
  }
  if (!resp.ok) {
    let msg = `HTTP ${resp.status}`
    try {
      const j = await resp.json()
      msg = j?.error?.message || msg
    } catch { /* 非 JSON 错误体 */ }
    throw new ApiError(msg, resp.status)
  }
  return resp.json()
}

// 带认证下载（附件/.eml 无法用 <a href>：需要 Authorization 头）
export async function download(path: string, filename: string) {
  const resp = await fetch(path, { headers: { Authorization: `Bearer ${token}` } })
  if (!resp.ok) throw new ApiError(`下载失败 HTTP ${resp.status}`, resp.status)
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

// SSE 事件流：新邮件到达即回调（载荷为邮件摘要 JSON 字符串：id/subject/from/account）。
// 浏览器断线自动重连，retry 5s；401 时浏览器停止重连。
// EventSource 无法带 Authorization 头，走 ?token= 查询参数（服务端单独认证）。
export function openEvents(onMessage: (data: string) => void): EventSource {
  const es = new EventSource(`/api/v1/events?token=${encodeURIComponent(token)}`)
  es.addEventListener('message.new', (ev) => { onMessage((ev as MessageEvent).data) })
  return es
}

// 相对时间显示（"3 分钟前"）
export function relTime(iso: string | null): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return iso
  const diff = Date.now() - t
  if (diff < 0) return new Date(iso).toLocaleString()
  const m = Math.floor(diff / 60000)
  if (m < 1) return '刚刚'
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d} 天前`
  return new Date(iso).toLocaleDateString()
}

export function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}
