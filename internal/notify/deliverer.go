// Package notify 实现 Webhook 投递器（DESIGN.md 5.2/5.3）。
// 语义：至少一次投递，下游以 delivery_id 幂等；轻载荷 + HMAC 签名；
// 重试 10s/1m/5m/30m/2h 共 5 次后置 failed，可手动重发。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/store"
)

// 重试间隔序列（DESIGN 5.3）；attempts 超过序列长度即终态 failed。
var retryDelays = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

const (
	batchSize    = 50               // 每轮取件上限
	wakeTick     = 15 * time.Second // 空闲轮询周期（兜底，正常由 Wake 触发）
	cleanupTick  = 6 * time.Hour    // 投递记录清理周期
	deliveryKeep = 30 * 24 * time.Hour
)

// Deliverer 扫描 pending 投递并并发 POST。实现 sync.Notifier（Wake）。
type Deliverer struct {
	store *store.Store
	cfg   *config.Config
	http  *http.Client

	wake chan struct{}
}

func New(cfg *config.Config, st *store.Store) *Deliverer {
	return &Deliverer{
		store: st,
		cfg:   cfg,
		http:  &http.Client{Timeout: 10 * time.Second}, // 请求超时 10s（DESIGN 5.3）
		wake:  make(chan struct{}, 1),
	}
}

// Wake 唤醒投递循环（IngestMessage 提交后由 sync 调用；非阻塞合并）。
func (d *Deliverer) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run 阻塞直至 ctx 取消。
func (d *Deliverer) Run(ctx context.Context) {
	slog.Info("webhook deliverer started", "concurrency", d.cfg.WebhookConcurrency)
	cleanup := time.NewTicker(cleanupTick)
	defer cleanup.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanup.C:
			if n, err := d.store.CleanOldDeliveries(deliveryKeep); err != nil {
				slog.Error("clean deliveries failed", "error", err)
			} else if n > 0 {
				slog.Info("cleaned old deliveries", "deleted", n)
			}
			continue
		case <-d.wake:
		case <-time.After(wakeTick):
		}
		d.pump(ctx)
	}
}

// pump 取一批到期待投递记录并发投递。
func (d *Deliverer) pump(ctx context.Context) {
	due, err := d.store.ListDueDeliveries(batchSize)
	if err != nil {
		slog.Error("list due deliveries failed", "error", err)
		return
	}
	if len(due) == 0 {
		return
	}
	if len(due) == batchSize {
		d.Wake() // 队列可能还有：立即再来一轮
	}

	sem := make(chan struct{}, d.cfg.WebhookConcurrency)
	var wg sync.WaitGroup
	for _, dv := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(dv *store.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := d.deliver(ctx, dv); err != nil {
				slog.Warn("webhook deliver error", "delivery_id", dv.ID, "error", err)
			}
		}(dv)
	}
	wg.Wait()
}

// deliver 单条投递并落结果。
func (d *Deliverer) deliver(ctx context.Context, dv *store.Delivery) error {
	wh, err := d.store.GetWebhook(dv.WebhookID)
	if err != nil {
		// webhook 已删除（delivery 本应级联删除，此处兜底）：置终态。
		return d.store.MarkDeliveryFailed(dv.ID, 0, "webhook 不存在", time.Time{}, true)
	}
	if !wh.Enabled {
		return nil // 禁用中：保持 pending，重新启用后继续投递
	}

	payload, err := d.buildPayload(wh, dv)
	if err != nil {
		// 载荷构建失败（如邮件已被删）：置终态 failed。
		return d.store.MarkDeliveryFailed(dv.ID, 0, err.Error(), time.Time{}, true)
	}

	code, err := d.post(ctx, wh, dv, payload)
	if err == nil { // 2xx
		return d.store.MarkDeliverySuccess(dv.ID, code)
	}

	attempts := dv.Attempts + 1
	exhausted := attempts >= len(retryDelays)+1
	var next time.Time
	if !exhausted {
		next = time.Now().UTC().Add(retryDelays[attempts-1])
	}
	if markErr := d.store.MarkDeliveryFailed(dv.ID, code, err.Error(), next, exhausted); markErr != nil {
		return markErr
	}
	if exhausted {
		return fmt.Errorf("投递 %d 重试耗尽置 failed: %w", dv.ID, err)
	}
	return err
}

// buildPayload 组装 message.received 载荷（DESIGN 5.2：轻载荷，元数据+摘要）。
func (d *Deliverer) buildPayload(wh *store.Webhook, dv *store.Delivery) ([]byte, error) {
	msg, err := d.store.GetMessage(dv.MessageID)
	if err != nil {
		return nil, fmt.Errorf("邮件不存在（id=%d）: %w", dv.MessageID, err)
	}
	acct, _ := d.store.GetAccount(msg.AccountID)

	type payloadMsg struct {
		ID             int64    `json:"id"`
		AccountID      int64    `json:"account_id"`
		AccountEmail   string   `json:"account_email"`
		Folder         string   `json:"folder"`
		Subject        string   `json:"subject"`
		From           string   `json:"from"`
		To             []string `json:"to,omitempty"`
		SentAt         *string  `json:"sent_at,omitempty"`
		ReceivedAt     string   `json:"received_at"`
		Snippet        string   `json:"snippet"`
		HasAttachments bool     `json:"has_attachments"`
		BodyText       string   `json:"body_text,omitempty"` // include_body 时附带（DESIGN 5.2）
	}
	m := payloadMsg{
		ID: msg.ID, AccountID: msg.AccountID, Folder: msg.Folder,
		Subject: msg.Subject, From: msg.FromAddr,
		ReceivedAt: msg.ReceivedAt.UTC().Format(time.RFC3339),
		Snippet:    msg.Snippet, HasAttachments: msg.HasAttachments,
	}
	if acct != nil {
		m.AccountEmail = acct.Email
	}
	if msg.ToAddrs != "" {
		_ = json.Unmarshal([]byte(msg.ToAddrs), &m.To)
	}
	if msg.SentAt.Valid {
		t := msg.SentAt.Time.UTC().Format(time.RFC3339)
		m.SentAt = &t
	}
	if wh.IncludeBody {
		m.BodyText = msg.BodyText
	}
	return json.Marshal(map[string]any{
		"event":       "message.received",
		"delivery_id": dv.ID,
		"message":     m,
	})
}

// post 签名并 POST；2xx 视为成功（DESIGN 5.2/5.3）。
func (d *Deliverer) post(ctx context.Context, wh *store.Webhook, dv *store.Delivery, payload []byte) (int, error) {
	return d.send(ctx, wh, dv.ID, payload)
}

// send 是唯一的签名+发送路径（正式投递与测试事件共用，DESIGN 5.2 请求格式）。
func (d *Deliverer) send(ctx context.Context, wh *store.Webhook, deliveryID int64, payload []byte) (int, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(wh.Secret))
	mac.Write([]byte(ts + "." + string(payload)))
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MailHub-Event", "message.received")
	req.Header.Set("X-MailHub-Delivery", strconv.FormatInt(deliveryID, 10))
	req.Header.Set("X-MailHub-Timestamp", ts)
	req.Header.Set("X-MailHub-Signature", "sha256="+sig)

	resp, err := d.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("下游返回 HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// PostTest 发送测试事件（DESIGN 6.4 POST /webhooks/{id}/test）：不进队列，直接投递一次。
func (d *Deliverer) PostTest(wh *store.Webhook) (int, error) {
	payload, err := json.Marshal(map[string]any{
		"event":       "message.received",
		"delivery_id": 0,
		"message": map[string]any{
			"id": 0, "account_id": 0, "account_email": "test@mailhub.local", "folder": "INBOX",
			"subject": "MailHub 测试事件", "from": "MailHub <test@mailhub.local>",
			"received_at": time.Now().UTC().Format(time.RFC3339),
			"snippet":     "这是一条测试事件，验证 webhook 配置与签名。", "has_attachments": false,
		},
	})
	if err != nil {
		return 0, err
	}
	return d.send(context.Background(), wh, 0, payload)
}
