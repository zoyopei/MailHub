// Package crypto 提供邮箱凭据的 AES-256-GCM 加解密（DESIGN.md 3.5、8）。
// 密文格式：12 字节 nonce || AES-GCM 密文（含认证标签）。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

type Cipher struct {
	aead cipher.AEAD
}

// New 以 32 字节主密钥（MAILHUB_SECRET_KEY 解码后）构建 Cipher。
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("密钥长度 %d，需要 32 字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt 加密并返回 nonce||ciphertext。
func (c *Cipher) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

// Decrypt 解密 Encrypt 的输出。密钥错误或数据被篡改时返回错误。
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns+c.aead.Overhead() {
		return nil, errors.New("密文长度不合法")
	}
	return c.aead.Open(nil, blob[:ns], blob[ns:], nil)
}
