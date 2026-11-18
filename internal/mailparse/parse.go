// Package mailparse 将原始 RFC822 字节解析为入库元数据（TECH.md §2.5）。
// 纯函数、无状态：解析失败不抛错，置 ParseError 并尽力填充（DESIGN.md 3.4——原文照存仍发通知）。
package mailparse

import (
	"bytes"
	"encoding/json"
	"net/mail"
	"strings"
	"time"

	"github.com/jhillyerd/enmime"
)

// Parsed 是解析结果，字段对齐 store.NewMessage 的输入。
type Parsed struct {
	MessageID      string
	Subject        string
	FromAddr       string
	ToAddrs        []string
	CcAddrs        []string
	SentAt         time.Time
	BodyText       string
	BodyHTML       string
	Snippet        string
	Size           int64
	Attachments    []Attachment
	HasAttachments bool // len(Attachments) > 0 的便捷位
	Truncated      bool
	ParseError     string
}

// Attachment 入库用的附件元数据。
type Attachment struct {
	Filename    string
	ContentType string
	Size        int64
	PartIndex   int
}

// Parse 解析原始邮件。maxSize 为单封上限（超出截断解析并标记 truncated，DESIGN.md 3.4）。
func Parse(raw []byte, maxSize int64) Parsed {
	p := Parsed{Size: int64(len(raw))}
	data := raw
	if maxSize > 0 && int64(len(raw)) > maxSize {
		data = raw[:maxSize]
		p.Truncated = true
	}

	env, err := enmime.ReadEnvelope(bytes.NewReader(data))
	if err != nil {
		// 头部整体不可读：只保留大小与截断标记，正文为空。
		p.ParseError = "读取 MIME 失败: " + err.Error()
		return p
	}

	p.MessageID = strings.TrimSpace(env.GetHeader("Message-ID"))
	p.Subject = env.GetHeader("Subject")
	// From 同 To/Cc：只存纯邮箱（显示名可能是 RFC2047 编码/引号包裹，显示不可靠）
	if addrs, err := env.AddressList("From"); err == nil && len(addrs) > 0 && addrs[0].Address != "" {
		p.FromAddr = addrs[0].Address
	}
	p.ToAddrs = addrStrings(env.AddressList("To"))
	p.CcAddrs = addrStrings(env.AddressList("Cc"))
	if t, err := env.Date(); err == nil {
		p.SentAt = t
	}

	p.BodyText = strings.ReplaceAll(env.Text, "\r\n", "\n")
	p.BodyHTML = env.HTML
	p.Snippet = snippet(p.BodyText, 200)

	for i, part := range env.Attachments {
		p.Attachments = append(p.Attachments, Attachment{
			Filename:    part.FileName,
			ContentType: part.ContentType,
			Size:        int64(len(part.Content)),
			PartIndex:   i + 1,
		})
	}
	p.HasAttachments = len(p.Attachments) > 0

	if len(env.Errors) > 0 {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, e.Error())
		}
		p.ParseError = strings.Join(msgs, "; ")
	}
	return p
}

// ToJSON 便捷输出收件人数组（store 存 JSON 字符串）。
func ToJSON(addrs []string) string {
	if len(addrs) == 0 {
		return ""
	}
	b, err := json.Marshal(addrs)
	if err != nil {
		return ""
	}
	return string(b)
}

// FindAttachment 下载时按 part_index 从 .eml 重新解析定位（DESIGN.md §4：附件不落盘）。
func FindAttachment(raw []byte, partIndex int) (filename, contentType string, content []byte, ok bool) {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return "", "", nil, false
	}
	for i, part := range env.Attachments {
		if i+1 == partIndex {
			return part.FileName, part.ContentType, part.Content, true
		}
	}
	return "", "", nil, false
}

func addrStrings(addrs []*mail.Address, err error) []string {
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		// 只存纯邮箱地址：名字可能是 RFC2047 编码/引号包裹，显示不可靠（用户约定：收发件人只看邮箱）
		if a.Address != "" {
			out = append(out, a.Address)
		}
	}
	return out
}

// snippet 压缩空白后取前 n 个 rune（DESIGN.md §4：正文前 200 字）。
func snippet(text string, n int) string {
	flat := strings.Join(strings.Fields(text), " ")
	runes := []rune(flat)
	if len(runes) <= n {
		return flat
	}
	return string(runes[:n]) + "…"
}
