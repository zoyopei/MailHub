// "今日收信"的日界：浏览器所在时区的本地零点（RFC3339）。
//
// 服务端缺省按它自己的时区算，容器里通常是 UTC——对东八区来说每天 00:00–08:00 算的还是"昨天"。
export function localDayStart(now: Date = new Date()): string {
  const d = new Date(now)
  d.setHours(0, 0, 0, 0)
  return d.toISOString()
}
