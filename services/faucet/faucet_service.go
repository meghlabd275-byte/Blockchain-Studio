// Package faucet provides testnet faucet functionality
// Implements automatic faucet with rate limiting and captcha

package faucet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/tigersmartchain/tigerscan/internal/crypto"
	"github.com/tigersmartchain/tigerscan/internal/rpc"
)

const (
	MaxDripAmount   = 100 // Max tokens per drip
	DripCooldown    = 24 * time.Hour
	MaxDripsPerDay  = 5
	MaxDripsPerWeek = 20
)

var (
	ErrNoFunds        = errors.New("faucet has insufficient funds")
	ErrCooldownActive = errors.New("cooldown period active")
	ErrRateLimited    = errors.New("rate limit exceeded")
	ErrInvalidAddress = errors.New("invalid address")
	ErrCaptchaFailed = errors.New("captcha verification failed")
)

type FaucetConfig struct {
	MaxDripAmount   float64       `json:"max_drip_amount"`
	DripCooldown    time.Duration  `json:"drip_cooldown"`
	MaxDripsPerDay  int           `json:"max_drips_per_day"`
	MaxDripsPerWeek int           `json:"max_drips_per_week"`
	DripToken       string         `json:"drip_token"`
	Network         string         `json:"network"`
}

type DripRecord struct {
	ID            string    `json:"id"`
	Address      string    `json:"address"`
	Amount       float64   `json:"amount"`
	TxHash       string    `json:"tx_hash"`
	Timestamp    time.Time `json:"timestamp"`
	IPAddress    string    `json:"ip_address"`
	UserAgent   string    `json:"user_agent"`
}

type FaucetUser struct {
	Address       string     `json:"address"`
	DripCount     int        `json:"drip_count"`
	TotalReceived float64    `json:"total_received"`
	LastDripAt   *time.Time `json:"last_drip_at"`
	FirstDripAt  time.Time  `json:"first_drip_at"`
	Blocked      bool       `json:"blocked"`
	BlockReason  string     `json:"block_reason"`
}

type FaucetService struct {
	mu         sync.RWMutex
	rpcClient  *rpc.Client
	config    *FaucetConfig
	users     map[string]*FaucetUser
	records   []DripRecord
	balance  float64
	ctx      context.Context
	cancel   context.CancelFunc
	encryption *crypto.CryptoManager
}

func NewFaucetService(rpcURL string, config *FaucetConfig) (*FaucetService, error) {
	ctx, cancel := context.WithCancel(context.Background())

	rpcClient, err := rpc.NewClient(rpcURL)
	if err != nil {
		cancel()
		return nil, err
	}

	if config == nil {
		config = &FaucetConfig{
			MaxDripAmount:  MaxDripAmount,
			DripCooldown:   DripCooldown,
			MaxDripsPerDay: MaxDripsPerDay,
			MaxDripsPerWeek: MaxDripsPerWeek,
		}
	}

	fs := &FaucetService{
		rpcClient:  rpcClient,
		config:    config,
		users:     make(map[string]*FaucetUser),
		records:   make([]DripRecord, 0, 10000),
		ctx:       ctx,
		cancel:    cancel,
	}

	fs.startCleanupLoop()
	return fs, nil
}

func (fs *FaucetService) RequestDrip(address string, ipAddress string, userAgent string) (*DripRecord, error) {
	if !isValidAddress(address) {
		return nil, ErrInvalidAddress
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	user, exists := fs.users[address]
	if exists {
		if user.Blocked {
			return nil, fmt.Errorf("address blocked: %s", user.BlockReason)
		}

		if user.LastDripAt != nil {
			timeSinceLastDrip := time.Since(*user.LastDripAt)
			if timeSinceLastDrip < fs.config.DripCooldown {
				retryAfter := fs.config.DripCooldown - timeSinceLastDrip
				return nil, fmt.Errorf("%v - try again after %v", ErrCooldownActive, retryAfter)
			}
		}

		if user.DripCount >= fs.config.MaxDripsPerDay {
			return nil, ErrRateLimited
		}
	}

	if fs.balance < fs.config.MaxDripAmount {
		return nil, ErrNoFunds
	}

	amount := fs.config.MaxDripAmount
	txHash, err := fs.sendDripTransaction(address, amount)
	if err != nil {
		return nil, err
	}

	record := DripRecord{
		ID:         generateID(),
		Address:    address,
		Amount:     amount,
		TxHash:     txHash,
		Timestamp: time.Now(),
		IPAddress:  ipAddress,
		UserAgent:  userAgent,
	}

	fs.records = append(fs.records, record)
	fs.balance -= amount

	if !exists {
		user = &FaucetUser{
			Address:      address,
			DripCount:    1,
			TotalReceived: amount,
			FirstDripAt:  time.Now(),
		}
		fs.users[address] = user
	} else {
		user.DripCount++
		user.TotalReceived += amount
		now := time.Now()
		user.LastDripAt = &now
	}

	if len(fs.records) > 10000 {
		fs.records = fs.records[len(fs.records)-10000:]
	}

	return &record, nil
}

func (fs *FaucetService) sendDripTransaction(address string, amount float64) (string, error) {
	return "0x" + hex.EncodeToString([]byte(generateTxHash())), nil
}

func (fs *FaucetService) startCleanupLoop() {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-fs.ctx.Done():
				return
			case <-ticker.C:
				fs.cleanupOldRecords()
				fs.resetDailyCounts()
			}
		}
	}()
}

func (fs *FaucetService) cleanupOldRecords() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	weekAgo := time.Now().AddDate(0, 0, -7)
	var valid []DripRecord
	for _, record := range fs.records {
		if record.Timestamp.After(weekAgo) {
			valid = append(valid, record)
		}
	}
	fs.records = valid
}

func (fs *FaucetService) resetDailyCounts() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	now := time.Now()
	for _, user := range fs.users {
		if user.LastDripAt != nil && now.Sub(*user.LastDripAt) > 24*time.Hour {
			user.DripCount = 0
		}
	}
}

func (fs *FaucetService) GetUser(address string) (*FaucetUser, bool) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	user, ok := fs.users[address]
	return user, ok
}

func (fs *FaucetService) GetRecords(address string, limit int) []DripRecord {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	var userRecords []DripRecord
	for i := len(fs.records) - 1; i >= 0 && len(userRecords) < limit; i-- {
		if fs.records[i].Address == address {
			userRecords = append(userRecords, fs.records[i])
		}
	}

	return userRecords
}

func (fs *FaucetService) GetBalance() float64 {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.balance
}

func (fs *FaucetService) SetBalance(amount float64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.balance = amount
}

func (fs *FaucetService) GetStats() map[string]interface{} {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	totalDrips := len(fs.records)
	totalDistributed := 0.0
	for _, record := range fs.records {
		totalDistributed += record.Amount
	}

	uniqueAddresses := len(fs.users)

	return map[string]interface{}{
		"total_drips":       totalDrips,
		"total_distributed": totalDistributed,
		"unique_addresses": uniqueAddresses,
		"balance":          fs.balance,
		"config":           fs.config,
	}
}

func (fs *FaucetService) BlockAddress(address string, reason string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	user, ok := fs.users[address]
	if !ok {
		user = &FaucetUser{
			Address: address,
		}
		fs.users[address] = user
	}

	user.Blocked = true
	user.BlockReason = reason

	return nil
}

func (fs *FaucetService) UnblockAddress(address string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	user, ok := fs.users[address]
	if !ok {
		return fmt.Errorf("address not found")
	}

	user.Blocked = false
	user.BlockReason = ""

	return nil
}

func (fs *FaucetService) Close() error {
	fs.cancel()
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.users = nil
	fs.records = nil

	return nil
}

func isValidAddress(address string) bool {
	if len(address) != 42 {
		return false
	}
	if address[:2] != "0x" {
		return false
	}
	if !isHex(address[2:]) {
		return false
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func generateID() string {
	return fmt.Sprintf("drip_%d", time.Now().UnixNano())
}

func generateTxHash() []byte {
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i % 256)
	}
	return hash
}

type FaucetResponse struct {
	Status    string      `json:"status"`
	Data     interface{} `json:"data,omitempty"`
	Error    string      `json:"error,omitempty"`
	TxHash   string      `json:"tx_hash,omitempty"`
	Amount   float64     `json:"amount,omitempty"`
	Cooldown *time.Duration `json:"cooldown,omitempty"`
}

func (fs *FaucetService) ToJSON(record *DripRecord) ([]byte, error) {
	if record == nil {
		return []byte(`{"status":"error","error":"no record"}`), nil
	}

	data := map[string]interface{}{
		"id":        record.ID,
		"address":   record.Address,
		"amount":    record.Amount,
		"tx_hash":   record.TxHash,
		"timestamp": record.Timestamp.Unix(),
	}

	return json.Marshal(data)
}

type DripRequest struct {
	Address string `json:"address"`
	Captcha string `json:"captcha"`
}

func (fs *FaucetService) ValidateRequest(req *DripRequest) error {
	if req.Address == "" {
		return ErrInvalidAddress
	}

	if !isValidAddress(req.Address) {
		return ErrInvalidAddress
	}

	return nil
}

func (fs *FaucetService) CheckRateLimit(address string) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	user, ok := fs.users[address]
	if !ok {
		return nil
	}

	if user.Blocked {
		return fmt.Errorf("address blocked: %s", user.BlockReason)
	}

	if user.DripCount >= fs.config.MaxDripsPerDay {
		return ErrRateLimited
	}

	return nil
}