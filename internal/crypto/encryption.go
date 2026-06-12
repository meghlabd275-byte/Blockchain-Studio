// Package crypto provides advanced cryptographic operations for TigerScan
// Implements AES-256-GCM, ChaCha20-Poly1305, and secure key derivation

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/pbkdf2"
	"hash"
	"sync"
	"time"
)

const (
	AESKeySize   = 32
	NonceSize    = 12
	TagSize     = 16
	Argon2Time  = 3
	Argon2Memory = 64 * 1024
	Argon2Threads = 4
	Argon2KeyLen = 32
	PBKDF2Iterations = 100000
	SaltSize = 32
)

var (
	ErrInvalidKeySize = errors.New("invalid key size")
	ErrDecryptionFailed = errors.New("decryption failed")
	ErrEncryptionFailed = errors.New("encryption failed")
)

	globalManager *CryptoManager
	managerOnce sync.Once
)

type CryptoManager struct {
	mu        sync.RWMutex
	masterKey []byte
	aesGCM    cipher.AEAD
	chaCha    *chacha20poly1305.X
	hashFunc  hash.Hash
}

func NewCryptoManager(masterKey []byte) (*CryptoManager, error) {
	if len(masterKey) != AESKeySize {
		return nil, ErrInvalidKeySize
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	chaCha, err := chacha20poly1305.NewX(masterKey[:])
	if err != nil {
		return nil, err
	}
	return &CryptoManager{
		masterKey: masterKey,
		aesGCM:    aesGCM,
		chaCha:    chaCha,
		hashFunc:  sha256.New(),
	}, nil
}

func GetGlobalManager() *CryptoManager {
	managerOnce.Do(func() {
		key := deriveKeyFromEnvironment()
		globalManager, _ = NewCryptoManager(key)
	})
	return globalManager
}

func deriveKeyFromEnvironment() []byte {
	salt := make([]byte, SaltSize)
	rand.Read(salt)
	input := make([]byte, 0, 256)
	key := pbkdf2.Key(input, salt, PBKDF2Iterations, AESKeySize, sha256.New)
	return key[:AESKeySize]
}

func (c *CryptoManager) EncryptAESGCM(plaintext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := c.aesGCM.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

func (c *CryptoManager) DecryptAESGCM(ciphertext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(ciphertext) < NonceSize {
		return nil, ErrDecryptionFailed
	}
	nonce := ciphertext[:NonceSize]
	data := ciphertext[NonceSize:]
	plaintext, err := c.aesGCM.Open(nil, nonce, data, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}

func (c *CryptoManager) EncryptChaCha20(plaintext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := c.chaCha.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

func (c *CryptoManager) DecryptChaCha20(ciphertext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(ciphertext) < chacha20poly1305.NonceSize {
		return nil, ErrDecryptionFailed
	}
	nonce := ciphertext[:chacha20poly1305.NonceSize]
	data := ciphertext[chacha20poly1305.NonceSize:]
	plaintext, err := c.chaCha.Open(nil, nonce, data, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}

func (c *CryptoManager) HashData(data []byte) []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	c.hashFunc.Reset()
	c.hashFunc.Write(data)
	return c.hashFunc.Sum(nil)
}

func (c *CryptoManager) HashDataString(data []byte) string {
	return base64.StdEncoding.EncodeToString(c.HashData(data))
}

func ConstantTimeCompare(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

func SecureZero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func DeriveKey(password string, salt []byte) []byte {
	return argon2.IDKey(password, salt, Argon2Time, Argon2Memory, Argon2Threads, Argon2KeyLen)
}

func GenerateSalt() ([]byte, error) {
	salt := make([]byte, SaltSize)
	_, err := rand.Read(salt)
	return salt, err
}

func DeriveKeyFromPassword(password []byte, salt []byte) []byte {
	return pbkdf2.Key(password, salt, PBKDF2Iterations, AESKeySize, sha256.New)
}

func GenerateNonce(size int) ([]byte, error) {
	nonce := make([]byte, size)
	_, err := rand.Read(nonce)
	return nonce, err
}

type KeyRotation struct {
	mu           sync.RWMutex
	currentKey   []byte
	previousKey  []byte
	rotationTime time.Time
	interval    time.Duration
}

func NewKeyRotation(interval time.Duration) (*KeyRotation, error) {
	key := make([]byte, AESKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &KeyRotation{
		currentKey:   key,
		rotationTime: time.Now(),
		interval:    interval,
	}, nil
}

func (kr *KeyRotation) RotateKey() error {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	kr.previousKey = kr.currentKey
	newKey := make([]byte, AESKeySize)
	if _, err := rand.Read(newKey); err != nil {
		return err
	}
	kr.currentKey = newKey
	kr.rotationTime = time.Now()
	return nil
}

func (kr *KeyRotation) ShouldRotate() bool {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	return time.Since(kr.rotationTime) >= kr.interval
}

func (kr *KeyRotation) GetCurrentKey() []byte {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	key := make([]byte, len(kr.currentKey))
	copy(key, kr.currentKey)
	return key
}

func HashPassword(password string) (string, error) {
	salt, err := GenerateSalt()
	if err != nil {
		return "", err
	}
	hash := DeriveKey(password, salt)
	hash = append(hash, salt...)
	return base64.StdEncoding.EncodeToString(hash), nil
}

func VerifyPassword(password, hashStr string) bool {
	hashBytes, err := base64.StdEncoding.DecodeString(hashStr)
	if err != nil {
		return false
	}
	if len(hashBytes) < SaltSize {
		return false
	}
	salt := hashBytes[len(hashBytes)-SaltSize:]
	derived := DeriveKey(password, salt)
	return ConstantTimeCompare(derived, hashBytes[:len(hashBytes)-SaltSize])
}

type SecureSession struct {
	id       string
	key      []byte
	created  time.Time
	expiry   time.Time
	data     map[string]interface{}
	mu       sync.RWMutex
}

func NewSecureSession(id string, duration time.Duration) *SecureSession {
	key := make([]byte, AESKeySize)
	rand.Read(key)
	return &SecureSession{
		id:      id,
		key:     key,
		created: time.Now(),
		expiry:  time.Now().Add(duration),
		data:   make(map[string]interface{}),
	}
}

func (s *SecureSession) IsExpired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Now().After(s.expiry)
}

func (s *SecureSession) Set(key string, value interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *SecureSession) Get(key string) (interface{}, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.data[key]
	return val, ok
}

func GenerateToken(length int) (string, error) {
	if length < 16 {
		length = 32
	}
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(data), nil
}

type EncryptedValue struct {
	Ciphertext []byte    `json:"ciphertext"`
	Nonce     []byte    `json:"nonce"`
	Algorithm string   `json:"algorithm"`
	Created   time.Time `json:"created"`
	KeyID     string    `json:"key_id"`
}

func NewEncryptedValue(data []byte) (*EncryptedValue, error) {
	manager := GetGlobalManager()
	if manager == nil {
		return nil, errors.New("crypto manager not initialized")
	}
	ciphertext, err := manager.EncryptAESGCM(data)
	if err != nil {
		return nil, err
	}
	return &EncryptedValue{
		Ciphertext: ciphertext,
		Algorithm: "AES-256-GCM",
		Created:   time.Now(),
		KeyID:     "default",
	}, nil
}

func (e *EncryptedValue) Decrypt() ([]byte, error) {
	manager := GetGlobalManager()
	if manager == nil {
		return nil, errors.New("crypto manager not initialized")
	}
	return manager.DecryptAESGCM(e.Ciphertext)
}

func Init() error {
	managerOnce.Do(func() {
		key := deriveKeyFromEnvironment()
		globalManager, _ = NewCryptoManager(key)
	})
	if globalManager == nil {
		return errors.New("failed to initialize crypto manager")
	}
	return nil
}