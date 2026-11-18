package mailparse

import (
	"strings"
	"testing"
)

// 样本 1：带附件的 multipart 邮件。
const sampleWithAttachment = `From: Service <noreply@service.com>
To: me@example.com
Cc: boss@example.com
Subject: =?UTF-8?B?6YKu5Lu26YWN572u?=
Date: Mon, 16 Nov 2026 12:55:30 +0800
Message-ID: <abc-123@service.com>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="BOUND"

--BOUND
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: base64

5L2g5aW977yM6YKu5Lu26YWN572u5p6QIDU1NSwgMTAg5YiG6ZKf5YmN5a6M5oiQ44CC
--BOUND
Content-Type: application/octet-stream; name="data.bin"
Content-Disposition: attachment; filename="data.bin"
Content-Transfer-Encoding: base64

QUJDREVGRw==
--BOUND--
`

// 样本 2：坏格式（无头无 MIME）。
const sampleGarbage = "not a mime message at all\x00\x01"

func TestParseBasicFields(t *testing.T) {
	p := Parse([]byte(sampleWithAttachment), 0)
	if p.MessageID != "<abc-123@service.com>" {
		t.Errorf("MessageID = %q", p.MessageID)
	}
	if !strings.Contains(p.Subject, "邮件配置") {
		t.Errorf("Subject(解码后) = %q", p.Subject)
	}
	if p.FromAddr != "noreply@service.com" {
		t.Errorf("From = %q（应为纯邮箱）", p.FromAddr)
	}
	if len(p.ToAddrs) != 1 || p.ToAddrs[0] != "me@example.com" {
		t.Errorf("To = %v", p.ToAddrs)
	}
	if len(p.CcAddrs) != 1 {
		t.Errorf("Cc = %v", p.CcAddrs)
	}
	if !strings.Contains(p.BodyText, "555") {
		t.Errorf("BodyText = %q", p.BodyText)
	}
	if p.Snippet == "" || len([]rune(p.Snippet)) > 202 {
		t.Errorf("Snippet = %q", p.Snippet)
	}
	if !p.HasAttachments || len(p.Attachments) != 1 {
		t.Fatalf("Attachments = %+v (HasAttachments=%v)", p.Attachments, p.HasAttachments)
	}
	if p.Attachments[0].Filename != "data.bin" || p.Attachments[0].Size != 7 {
		t.Errorf("attachment = %+v", p.Attachments[0])
	}
	if p.ParseError != "" {
		t.Errorf("意外解析错误: %s", p.ParseError)
	}
	if p.SentAt.IsZero() {
		t.Errorf("SentAt 未解析")
	}
}

func TestParseGarbage(t *testing.T) {
	p := Parse([]byte(sampleGarbage), 0)
	if p.ParseError == "" {
		t.Errorf("坏格式应置 ParseError")
	}
	if p.Size != int64(len(sampleGarbage)) {
		t.Errorf("Size = %d", p.Size)
	}
}

// From/To/Cc 只保留纯邮箱：带显示名（含 RFC2047 编码名）的地址剥掉名字部分。
func TestParseAddrsEmailOnly(t *testing.T) {
	raw := `From: =?UTF-8?B?5L2g5a2m?= 张三 <zhang@qq.com>
To: 李四 <lisi@qq.com>, =?UTF-8?B?546w5LiA?= <wang@163.com>, plain@x.com
Cc: boss@corp.com
Subject: addr test

body
`
	p := Parse([]byte(raw), 0)
	if p.FromAddr != "zhang@qq.com" {
		t.Errorf("From = %q（应剥掉编码显示名）", p.FromAddr)
	}
	want := []string{"lisi@qq.com", "wang@163.com", "plain@x.com"}
	if len(p.ToAddrs) != len(want) {
		t.Fatalf("To = %v（应为 %v）", p.ToAddrs, want)
	}
	for i, w := range want {
		if p.ToAddrs[i] != w {
			t.Errorf("To[%d] = %q, want %q", i, p.ToAddrs[i], w)
		}
	}
	if len(p.CcAddrs) != 1 || p.CcAddrs[0] != "boss@corp.com" {
		t.Errorf("Cc = %v", p.CcAddrs)
	}
}

func TestParseTruncated(t *testing.T) {
	p := Parse([]byte(sampleWithAttachment), 64) // 远小于原文
	if !p.Truncated {
		t.Errorf("超限应标记 Truncated")
	}
	if p.Size != int64(len(sampleWithAttachment)) {
		t.Errorf("Size 应为原始大小 %d，得 %d", len(sampleWithAttachment), p.Size)
	}
}

func TestFindAttachment(t *testing.T) {
	_, _, content, ok := FindAttachment([]byte(sampleWithAttachment), 1)
	if !ok || string(content) != "ABCDEFG" {
		t.Errorf("FindAttachment = %q ok=%v", content, ok)
	}
	if _, _, _, ok := FindAttachment([]byte(sampleWithAttachment), 9); ok {
		t.Errorf("不存在的 part 应返回 false")
	}
}

func TestToJSON(t *testing.T) {
	if ToJSON(nil) != "" {
		t.Error("空列表应为空串")
	}
	if got := ToJSON([]string{"a@b.c"}); got != `["a@b.c"]` {
		t.Errorf("ToJSON = %s", got)
	}
}
