// 邮件列表的纯逻辑：不碰 DOM、不发请求，可以直接用 node --test 单测。
// 组件（MailView.vue）只负责把这些函数接到响应式状态和真实 DOM 上。

export type SortKey = 'default' | 'time' | 'account'
export type SortOrder = 'asc' | 'desc'

export interface ListFilter {
  accountId: number // 0 = 全部账号
  q: string
  onlyUnread: boolean
  tags: readonly string[]
  /** [起始, 结束]，毫秒时间戳 */
  timeRange: readonly [number, number] | null
  sort: SortKey
  order: SortOrder
}

export interface Page {
  limit: number
  /** 默认排序：从哪一封（不含）往前取 */
  beforeId?: number
  /** 排序模式：偏移量 */
  offset?: number
  /** 追加加载时跳过总数统计 */
  noTotal?: boolean
}

export interface HasID { id: number }

/** 列表每页上限，也是服务端允许的最大 limit */
export const MAX_PAGE = 200
const FIRST_PAGE = 50

/**
 * GET /api/v1/messages 的查询参数。两套分页互斥：排序模式（时间/邮箱）走 offset，
 * 默认模式走 before_id 游标——混用会漏行或重复。
 */
export function buildListQuery(f: ListFilter, pg: Page): URLSearchParams {
  const p = new URLSearchParams()
  if (f.accountId) p.set('account_id', String(f.accountId))
  const q = f.q.trim()
  if (q) p.set('q', q)
  if (f.onlyUnread) p.set('is_read', 'false')
  f.tags.forEach(t => p.append('tag', t))
  if (f.timeRange) {
    p.set('since', new Date(f.timeRange[0]).toISOString())
    p.set('until', new Date(f.timeRange[1]).toISOString())
  }
  p.set('limit', String(pg.limit))
  if (f.sort !== 'default') {
    p.set('sort', f.sort)
    p.set('order', f.order)
    if (pg.offset) p.set('offset', String(pg.offset))
  } else if (pg.beforeId) {
    p.set('before_id', String(pg.beforeId))
  }
  if (pg.noTotal) p.set('no_total', '1')
  return p
}

/** 原地刷新要重取的行数：至少一页（新邮件可能不止一封），最多 MAX_PAGE。 */
export function refreshLimit(loaded: number): number {
  return Math.min(Math.max(loaded, FIRST_PAGE), MAX_PAGE)
}

/**
 * 原地刷新的结果合并。head 是重新取回的第一页（limit 行），tail 是刷新前已加载的行：
 * 首页就是全部（没取满一页，或已覆盖总数）时以它为准——顺带清掉被别处删掉的旧行；
 * 否则首页替换旧的头部，旧列表里排在首页之后的行原样保留（它们不在这次请求范围内）。
 */
export function mergeRefreshed<T extends HasID>(head: T[], limit: number, total: number, tail: T[]): T[] {
  if (head.length < limit || head.length >= total) return head
  const seen = new Set(head.map(r => r.id))
  return head.concat(tail.filter(r => !seen.has(r.id)))
}

/** 把勾选修剪到仍然可见的行上：列表刷新后有些邮件可能已经不在了。 */
export function pruneChecked(checked: ReadonlySet<number>, visible: Iterable<number>): Set<number> {
  const keep = new Set(visible)
  return new Set([...checked].filter(id => keep.has(id)))
}

export interface RowBox { id: number; top: number; bottom: number } // 相对滚动容器顶缘的像素
export interface Anchor { id: number; top: number }

/**
 * 取"视口里的第一行"及其后 n-1 行作锚点。第一行被新邮件挤走或被删掉时，后备锚点还能把
 * 滚动位置找回来。headerBottom 是吸顶表头的下缘：被它盖住的行不算在视口里。
 */
export function pickAnchors(rows: readonly RowBox[], headerBottom: number, n = 5): Anchor[] {
  const i = rows.findIndex(r => r.bottom > headerBottom + 1)
  if (i < 0) return []
  return rows.slice(i, i + n).map(({ id, top }) => ({ id, top }))
}

/**
 * 让锚点回到原位所需的滚动位移：用第一个仍然存在的锚点算。锚点全都不在了返回 null
 * （调用方保持当前滚动位置不动）。
 */
export function anchorDelta(anchors: readonly Anchor[], rows: readonly RowBox[]): number | null {
  for (const a of anchors) {
    const r = rows.find(x => x.id === a.id)
    if (r) return r.top - a.top
  }
  return null
}

/**
 * 逐页取完整个列表。一页最多 200 个，矩阵上千个账号时只取第一页会让后面的账号选不到、
 * 合计也只算了前 200 个。fetchPage(page) 的 page 从 1 起；任何一页失败即整体失败。
 * 翻页期间账号有增删时可能重复或漏掉：按 id 去重兜底；某一页没带来新行就停，不空转翻页。
 */
export async function fetchAllPages<T extends HasID>(
  fetchPage: (page: number) => Promise<{ total: number; accounts: T[] }>,
): Promise<{ total: number; accounts: T[] }> {
  const byID = new Map<number, T>()
  let total = 0
  for (let page = 1; page <= 50; page++) { // 50 页 = 1 万个账号：防服务端 total 异常时无限翻页
    const resp = await fetchPage(page)
    total = resp.total // 以最后一次响应为准（翻页期间可能有增删）
    const seen = byID.size
    for (const a of resp.accounts) byID.set(a.id, a)
    if (resp.accounts.length === 0 || byID.size >= resp.total || byID.size === seen) break
  }
  return { total, accounts: [...byID.values()] }
}
