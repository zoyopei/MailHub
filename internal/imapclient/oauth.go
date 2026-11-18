package imapclient

// 令牌刷新（TokenCache）已迁移至 internal/mstoken：IMAP 与 Graph 需要**不同
// audience** 的令牌（main.py 实测，DESIGN.md 3.5/3.6），scope 必须参数化。
// 本文件只保留 XOAUTH2 SASL 实现。

// xoauth2 实现 sasl.Client（go-sasl 未提供 XOAUTH2，仅 OAUTHBEARER——自实现，
// 协议极简：initial response 即 "user=…\x01auth=Bearer …\x01\x01"）。
type xoauth2 struct {
	user, token string
}

func newXOAUTH2(user, token string) *xoauth2 { return &xoauth2{user: user, token: token} }

func (x *xoauth2) Start() (mech string, ir []byte, err error) {
	ir = []byte("user=" + x.user + "\x01auth=Bearer " + x.token + "\x01\x01")
	return "XOAUTH2", ir, nil
}

// Next 处理服务器错误 challenge（base64 JSON）：发送空响应终止协商，
// 让服务器返回 NO——错误细节由上层 Connect 包装。
func (x *xoauth2) Next(challenge []byte) ([]byte, error) { return []byte{}, nil }
