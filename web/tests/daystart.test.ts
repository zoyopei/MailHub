import { test } from 'node:test'
import assert from 'node:assert/strict'
import { localDayStart } from '../src/lib/daystart.ts'

test('localDayStart 是浏览器本地时区的当天零点（RFC3339）', () => {
  const now = new Date(2026, 10, 24, 15, 34, 56, 789) // 按本地时间构造：2026-11-24 15:34:56.789
  const s = localDayStart(now)
  const d = new Date(s)

  assert.equal(d.getFullYear(), 2026)
  assert.equal(d.getMonth(), 10)
  assert.equal(d.getDate(), 24)
  assert.equal(d.getHours(), 0)
  assert.equal(d.getMinutes(), 0)
  assert.equal(d.getSeconds(), 0)
  assert.equal(d.getMilliseconds(), 0)
  assert.equal(now.getTime() - d.getTime(), (15 * 3600 + 34 * 60 + 56) * 1000 + 789)
  assert.match(s, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/) // 服务端按 RFC3339 解析
})

test('localDayStart 不改传入的 Date', () => {
  const now = new Date(2026, 0, 15, 12, 0, 0)
  localDayStart(now)
  assert.equal(now.getHours(), 12)
})
