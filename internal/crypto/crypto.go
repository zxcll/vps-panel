// Package crypto 用 AES-256-GCM 加密落库的敏感字段（SSH 密码/私钥、DNS API 凭据）。
//
// 密文格式：nonce(12B) || ciphertext || tag(16B)，直接存 BLOB。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

var ErrNoKey = errors.New("加密器未初始化：缺少主密钥")

type Cipher struct {
	aead cipher.AEAD
}

// New 用 32 字节主密钥构造加密器。
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥必须是 32 字节，当前 %d 字节", len(key))
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

// Encrypt 加密明文。空明文返回 nil，方便"字段未设置"和"字段是空串"用同一种表示。
func (c *Cipher) Encrypt(plaintext string) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, ErrNoKey
	}
	if plaintext == "" {
		return nil, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("生成 nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt 解密。传入 nil/空切片返回空串。
func (c *Cipher) Decrypt(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	if c == nil || c.aead == nil {
		return "", ErrNoKey
	}
	ns := c.aead.NonceSize()
	if len(blob) < ns+c.aead.Overhead() {
		return "", errors.New("密文长度不足，数据可能已损坏")
	}
	plain, err := c.aead.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（主密钥可能已更换）: %w", err)
	}
	return string(plain), nil
}
