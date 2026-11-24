import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  MAX_PAGE, anchorDelta, buildListQuery, fetchAllPages, mergeRefreshed, pickAnchors,
  pruneChecked, refreshLimit,
} from '../src/lib/maillist.ts'

const base = {
  accountId: 0, q: '', onlyUnread: false, tags: [], timeRange: null,
  sort: 'default', order: 'desc',
} as const

const ids = (rows: { id: number }[]) => rows.map(r => r.id)
const row = (id: number) => ({ id })

test('buildListQuery：默认筛选只有 limit', () => {
  const p = buildListQuery(base, { limit: 50 })
  assert.deepEqual([...p], [['limit', '50']])
})

test('buildListQuery：筛选条件', () => {
  const p = buildListQuery({
    accountId: 7, q: '  验证码 ', onlyUnread: true, tags: ['a', 'b'],
    timeRange: [Date.UTC(2026, 10, 1), Date.UTC(2026, 10, 2)], sort: 'default', order: 'desc',
  }, { limit: 50 })
  assert.equal(p.get('account_id'), '7')
  assert.equal(p.get('q'), '验证码') // 去首尾空白
  assert.equal(p.get('is_read'), 'false')
  assert.deepEqual(p.getAll('tag'), ['a', 'b']) // 可重复
  assert.equal(p.get('since'), '2026-11-01T00:00:00.000Z')
  assert.equal(p.get('until'), '2026-11-02T00:00:00.000Z')
  assert.ok(!p.has('before_id') && !p.has('offset') && !p.has('no_total'))
})

test('buildListQuery：默认排序走 before_id 游标，不带 offset', () => {
  const p = buildListQuery(base, { limit: 50, beforeId: 1001, noTotal: true })
  assert.equal(p.get('before_id'), '1001')
  assert.ok(!p.has('offset') && !p.has('sort') && !p.has('order'))
  assert.equal(p.get('no_total'), '1')
})

test('buildListQuery：排序模式走 offset，不带 before_id', () => {
  for (const [sort, order] of [['time', 'asc'], ['account', 'desc']] as const) {
    const p = buildListQuery({ ...base, sort, order }, { limit: 50, offset: 150, noTotal: true })
    assert.equal(p.get('sort'), sort)
    assert.equal(p.get('order'), order)
    assert.equal(p.get('offset'), '150')
    assert.ok(!p.has('before_id'))
  }
})

test('buildListQuery：可选分页参数缺省时不出现在查询串里', () => {
  const p = buildListQuery({ ...base, sort: 'time', order: 'asc' }, { limit: 200 })
  const q = p.toString()
  assert.ok(!q.includes('undefined') && !q.includes('offset') && !q.includes('no_total'), q)
})

test('refreshLimit：至少一页、最多 MAX_PAGE', () => {
  assert.deepEqual([0, 1, 49, 50, 51, 120, 200, 500].map(refreshLimit),
    [50, 50, 50, 50, 51, 120, 200, 200])
  assert.equal(MAX_PAGE, 200)
})

test('mergeRefreshed：首页没取满 = 全部结果，以首页为准（清掉被别处删掉的旧行）', () => {
  const head = [row(9), row(5)]
  assert.deepEqual(ids(mergeRefreshed(head, 50, 2, [row(5), row(4), row(3)])), [9, 5])
})

test('mergeRefreshed：首页已覆盖总数时同样以首页为准', () => {
  const head = [row(3), row(2), row(1)]
  assert.deepEqual(ids(mergeRefreshed(head, 3, 3, [row(3), row(2), row(1)])), [3, 2, 1])
})

test('mergeRefreshed：取满一页时替换头部，旧列表中排在首页之后的行保留', () => {
  // 旧列表 [5,6,4,3]（4 行）。新邮件 9 插到最前，整体后移一行；这次只重取前 2 行 [9,5]，
  // 6 现在排在首页之后，从旧列表原样接上——拼出来的正是完整的新列表
  const out = mergeRefreshed([row(9), row(5)], 2, 5, [row(5), row(6), row(4), row(3)])
  assert.deepEqual(ids(out), [9, 5, 6, 4, 3])
})

test('mergeRefreshed：与首页重叠的旧行不重复', () => {
  const out = mergeRefreshed([row(9), row(8), row(5)], 3, 6, [row(5), row(4), row(3), row(2)])
  assert.deepEqual(ids(out), [9, 8, 5, 4, 3, 2])
})

test('mergeRefreshed：刷新后列表为空', () => {
  assert.deepEqual(mergeRefreshed([], 50, 0, [row(1)]), [])
})

test('pruneChecked：只留仍可见的', () => {
  const checked = new Set([1, 2, 3])
  const out = pruneChecked(checked, [2, 3, 4])
  assert.deepEqual([...out].sort(), [2, 3])
  assert.deepEqual([...checked].sort(), [1, 2, 3], '返回新集合，不改原集合')
  assert.deepEqual([...pruneChecked(checked, [])], [])
})

test('pickAnchors：视口里的第一行（被吸顶表头盖住一半的也算）及其后几行', () => {
  const rows = [
    { id: 1, top: -10, bottom: 20 },  // 整行在表头之下，看不见
    { id: 2, top: 20, bottom: 60 },   // 下半截露在表头下缘（30）之外 → 视口第一行
    { id: 3, top: 60, bottom: 100 },
    { id: 4, top: 100, bottom: 140 },
    { id: 5, top: 140, bottom: 180 },
    { id: 6, top: 180, bottom: 220 },
    { id: 7, top: 220, bottom: 260 }, // 默认只取 5 个锚点
    { id: 8, top: 260, bottom: 300 },
  ]
  assert.deepEqual(pickAnchors(rows, 30), [
    { id: 2, top: 20 }, { id: 3, top: 60 }, { id: 4, top: 100 },
    { id: 5, top: 140 }, { id: 6, top: 180 },
  ])
  // 行数不足时有多少取多少
  assert.deepEqual(pickAnchors(rows.slice(0, 3), 30), [{ id: 2, top: 20 }, { id: 3, top: 60 }])
  // 没有表头：第一行就是锚点
  assert.deepEqual(pickAnchors(rows, 0).slice(0, 2), [{ id: 1, top: -10 }, { id: 2, top: 20 }])
})

test('pickAnchors：视口里没有行时为空', () => {
  assert.deepEqual(pickAnchors([{ id: 1, top: -40, bottom: 30 }], 30), [])
  assert.deepEqual(pickAnchors([], 30), [])
})

test('anchorDelta：第一行下移多少，滚动位置就跟着移多少', () => {
  // 刷新前第一行 top=40；刷新后它上面插了一封 30px 高的新邮件 → 下移 30
  const delta = anchorDelta([{ id: 1, top: 40 }, { id: 2, top: 80 }],
    [{ id: 9, top: 0, bottom: 30 }, { id: 1, top: 70, bottom: 110 }, { id: 2, top: 110, bottom: 150 }])
  assert.equal(delta, 30)
})

test('anchorDelta：原来的第一行被删掉时用后备锚点', () => {
  const delta = anchorDelta([{ id: 1, top: 40 }, { id: 2, top: 80 }],
    [{ id: 9, top: 0, bottom: 40 }, { id: 2, top: 100, bottom: 140 }])
  assert.equal(delta, 20)
})

test('anchorDelta：锚点全不在或没有锚点时不动', () => {
  assert.equal(anchorDelta([{ id: 1, top: 40 }], [{ id: 9, top: 0, bottom: 40 }]), null)
  assert.equal(anchorDelta([], [{ id: 9, top: 0, bottom: 40 }]), null)
})

const acct = (id: number) => ({ id, email: `a${id}@x.com` })

test('fetchAllPages：一页装得下只请求一次', async () => {
  const calls: number[] = []
  const out = await fetchAllPages(async page => {
    calls.push(page)
    return { total: 3, accounts: [acct(1), acct(2), acct(3)] }
  })
  assert.deepEqual(calls, [1])
  assert.equal(out.total, 3)
  assert.deepEqual(ids(out.accounts), [1, 2, 3])
})

test('fetchAllPages：逐页取完，页码从 1 连续递增', async () => {
  // 服务端每页 2 条、共 5 条
  const all = [1, 2, 3, 4, 5].map(acct)
  const calls: number[] = []
  const out = await fetchAllPages(async page => {
    calls.push(page)
    return { total: 5, accounts: all.slice((page - 1) * 2, page * 2) }
  })
  assert.deepEqual(calls, [1, 2, 3])
  assert.equal(out.total, 5)
  assert.deepEqual(ids(out.accounts), [1, 2, 3, 4, 5])
})

test('fetchAllPages：恰好整数页时不多发一次空请求', async () => {
  const calls: number[] = []
  const out = await fetchAllPages(async page => {
    calls.push(page)
    return { total: 4, accounts: [acct(page * 2 - 1), acct(page * 2)] }
  })
  assert.deepEqual(calls, [1, 2])
  assert.deepEqual(ids(out.accounts), [1, 2, 3, 4])
})

test('fetchAllPages：翻页期间有新账号插入（行往后挪）不重复', async () => {
  // 模拟服务端按 offset 翻页、每页 2 条：第 1 页取到 [1,2] 之后，0 号账号插到了最前
  let list = [1, 2, 3].map(acct)
  const calls: number[] = []
  const out = await fetchAllPages(async page => {
    calls.push(page)
    if (page === 2) list = [acct(0), ...list]
    return { total: list.length, accounts: list.slice((page - 1) * 2, page * 2) }
  })
  // 2 号在两页里都出现，只留一份；0 号挪到了已翻过的窗口之前，这次漏掉（下次刷新补上）
  assert.deepEqual(calls, [1, 2, 3])
  assert.deepEqual(ids(out.accounts), [1, 2, 3])
  assert.equal(out.total, 4)
})

test('fetchAllPages：某一页没有带来新行就停（不空转翻页）', async () => {
  let calls = 0
  const out = await fetchAllPages(async () => {
    calls++
    return { total: 1000, accounts: [acct(1), acct(2)] } // 每页都是同样两行
  })
  assert.equal(calls, 2)
  assert.deepEqual(ids(out.accounts), [1, 2])
})

test('fetchAllPages：某一页失败则整体失败（不拿半份列表充数）', async () => {
  await assert.rejects(fetchAllPages(async page => {
    if (page === 3) throw new Error('HTTP 500')
    return { total: 1000, accounts: [acct(page)] }
  }), /HTTP 500/)
})

test('fetchAllPages：服务端 total 异常（永远取不完）时到 50 页为止', async () => {
  let calls = 0
  const out = await fetchAllPages(async page => {
    calls++
    return { total: Number.MAX_SAFE_INTEGER, accounts: [acct(page * 2 - 1), acct(page * 2)] }
  })
  assert.equal(calls, 50)
  assert.equal(out.accounts.length, 100)
})

test('fetchAllPages：空页即停', async () => {
  const calls: number[] = []
  const out = await fetchAllPages(async page => {
    calls.push(page)
    return { total: 0, accounts: [] }
  })
  assert.deepEqual(calls, [1])
  assert.deepEqual(out, { total: 0, accounts: [] })
})
