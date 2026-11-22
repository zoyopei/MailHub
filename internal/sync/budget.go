package sync

import "sync"

// idleBudget 单 IMAP 主机 IDLE 长连接预算（DESIGN.md 2.2）：
// 矩阵账号常大量落在同一服务商，超出预算的账号自动降级轮询，
// 避免触发服务商对单 IP 的连接数风控。
// limit 由调用方每次传入（全局配置 DB 覆盖，运行时可改）；limit <= 0 表示不限。
type idleBudget struct {
	mu     sync.Mutex
	active map[string]int
}

func newIdleBudget() *idleBudget {
	return &idleBudget{active: map[string]int{}}
}

// Acquire 尝试占用一个名额（limit 为当前生效预算）；成功返回 true
// （调用方在不再 IDLE 时 Release）。nil 接收者视为不限。
// 无论 limit 是否 <= 0 均递增计数，保证 Acquire/Release 1:1 配平，
// 运行时从不限（0）切到有限额时也能准确反映当前活跃连接数。
func (b *idleBudget) Acquire(host string, limit int) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit > 0 && b.active[host] >= limit {
		return false
	}
	b.active[host]++
	return true
}

// Release 归还名额。
func (b *idleBudget) Release(host string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active[host] > 0 {
		b.active[host]--
	}
}
