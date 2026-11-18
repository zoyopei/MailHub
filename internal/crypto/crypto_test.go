package crypto

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"", "p@ssw0rd", "O.XXXXX-refresh-token-很长的一段令牌内容"} {
		blob, err := c.Encrypt([]byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Decrypt(blob)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != plain {
			t.Fatalf("往返不一致: %q != %q", got, plain)
		}
	}
}

func TestKeyMismatch(t *testing.T) {
	c1, _ := New([]byte("01234567890123456789012345678901"))
	c2, _ := New([]byte("abcdefghij0123456789abcdefghij01"))
	blob, _ := c1.Encrypt([]byte("secret"))
	if _, err := c2.Decrypt(blob); err == nil {
		t.Fatal("不同密钥应解密失败")
	}
}

func TestTamperDetection(t *testing.T) {
	c, _ := New(make([]byte, 32))
	blob, _ := c.Encrypt([]byte("secret"))
	blob[len(blob)-1] ^= 0xFF
	if _, err := c.Decrypt(blob); err == nil {
		t.Fatal("篡改密文应解密失败（GCM 认证）")
	}
}

func TestNonceUniqueness(t *testing.T) {
	c, _ := New(make([]byte, 32))
	a, _ := c.Encrypt([]byte("same"))
	b, _ := c.Encrypt([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("相同明文两次加密应产生不同密文（随机 nonce）")
	}
}
