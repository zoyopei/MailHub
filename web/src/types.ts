// 与后端 internal/api 的 JSON 响应对应的类型定义。
export interface Account {
  id: number
  name: string
  email: string
  imap_host: string
  imap_port: number
  username: string
  auth_type: string
  client_id: string
  transport: string
  folders: string[]
  poll_interval: number
  enabled: boolean
  notify_enabled: boolean
  status: string
  last_error: string | null
  last_sync_at: string | null
  last_message_at: string | null
  message_count: number
  unread_count: number
  tags: string[]
}

export interface AccountListResp { total: number; accounts: Account[] }

export interface TagInfo { name: string; count: number }
export interface TagListResp { tags: TagInfo[] }

// 全局配置（GET/PUT /api/v1/settings）：value 为生效值（DB 覆盖 > 环境变量 > 默认），
// overridden 表示该项当前被数据库覆盖（可清除回退环境变量层）。
export interface SettingItem { value: number; overridden: boolean }
export interface SettingsResp { settings: Record<string, SettingItem> }

export interface Msg {
  id: number
  account_id: number
  folder: string
  subject: string
  from: string
  to: string[] | null
  sent_at: string | null
  received_at: string
  snippet: string
  has_attachments: boolean
  is_read: boolean
  truncated: boolean
  tags: string[]
}

export interface Att {
  id: number
  filename: string
  content_type: string
  size: number
}

export interface MsgDetail extends Msg {
  body_text: string
  body_html: string
  parse_error?: string
  attachments: Att[] | null
}

export interface StatusResp {
  accounts_by_status: Record<string, number>
  deliveries_pending: number
  deliveries_failed: number
  disk_usage_bytes: number
  messages_today: number
}
