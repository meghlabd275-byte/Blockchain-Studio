// Package api provides tiered API system for TigerScan
// Implements Free/Pro/Enterprise tiers with advanced rate limiting

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/tigersmartchain/tigerscan/internal/crypto"
	"github.com/tigersmartchain/tigerscan/internal/rpc"
)

const (
	FreeTier    API Tier = "free"
	ProTier    API Tier = "pro"
	Enterprise API Tier = "enterprise"
)

var (
	ErrInvalidTier      = errors.New("invalid API tier")
	ErrQuotaExceeded   = errors.New("API quota exceeded")
	ErrInvalidAPIKey   = errors.New("invalid API key")
	ErrKeyRevoked     = errors.New("API key revoked")
	ErrTierUpgrade    = errors.New("tier upgrade required")
)

type API string

type TierConfig struct {
	Name           string        `json:"name"`
	RateLimit     int           `json:"rate_limit"`
	RateLimitUnit time.Duration `json:"rate_limit_unit"`
	DailyQuota    int           `json:"daily_quota"`
	MonthlyQuota int           `json:"monthly_quota"`
	Features     []string      `json:"features"`
	Support      string        `json:"support"`
	Price        float64       `json:"price"`
}

var TierConfigs = map[API]TierConfig{
	FreeTier: {
		Name:           "Free",
		RateLimit:      5,
		RateLimitUnit: time.Second,
		DailyQuota:    1000,
		MonthlyQuota: 30000,
		Features:     []string{"basic_blocks", "basic_transactions", "basic_tokens"},
		Support:      "community",
		Price:        0,
	},
	ProTier: {
		Name:           "Pro",
		RateLimit:      100,
		RateLimitUnit: time.Second,
		DailyQuota:     100000,
		MonthlyQuota:  3000000,
		Features:      []string{"basic_blocks", "basic_transactions", "basic_tokens", "advanced_tokens", "nft_data", "gas_oracle"},
		Support:       "email",
		Price:         99,
	},
	Enterprise: {
		Name:           "Enterprise",
		RateLimit:      1000,
		RateLimitUnit:  time.Second,
		DailyQuota:     10000000,
		MonthlyQuota:  300000000,
		Features:      []string{"all_features", "custom_indexing", "webhooks", "dedicated_support", "sla"},
		Support:       "24/7",
		Price:         999,
	},
}

type APIKey struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Tier        API       `json:"tier"`
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	UsageCount  int       `json:"usage_count"`
	DailyUsage  int       `json:"daily_usage"`
	MonthlyUsage int     `json:"monthly_usage"`
	Metadata   map[string]interface{} `json:"metadata"`
}

type UsageRecord struct {
	Timestamp   time.Time `json:"timestamp"`
	Endpoint   string    `json:"endpoint"`
	Method     string    `json:"method"`
	StatusCode int      `json:"status_code"`
	Latency    time.Duration `json:"latency"`
	BytesIn    int       `json:"bytes_in"`
	BytesOut   int       `json:"bytes_out"`
}

type RateLimiter struct {
	mu           sync.RWMutex
	requests     map[string][]time.Time
	dailyUsage   map[string]int
	monthlyUsage map[string]int
	config      map[API]TierConfig
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		requests:     make(map[string][]time.Time),
		dailyUsage:   make(map[string]int),
		monthlyUsage: make(map[string]int),
		config:      TierConfigs,
	}
}

func (rl *RateLimiter) AllowRequest(apiKey *APIKey) error {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	config, ok := rl.config[apiKey.Tier]
	if !ok {
		return ErrInvalidTier
	}

	now := time.Now()
	requests := rl.requests[apiKey.Key]
	
	var recentRequests int
	for _, t := range requests {
		if now.Sub(t) < config.RateLimitUnit {
			recentRequests++
		}
	}

	if recentRequests >= config.RateLimit {
		return ErrQuotaExceeded
	}

	if apiKey.DailyUsage >= config.DailyQuota {
		return ErrQuotaExceeded
	}

	rl.requests[apiKey.Key] = append(requests, now)
	apiKey.DailyUsage++
	apiKey.UsageCount++

	rl.cleanupOldRequests(apiKey.Key)

	return nil
}

func (rl *RateLimiter) cleanupOldRequests(key string) {
	now := time.Now()
	requests := rl.requests[key]
	
	var valid []time.Time
	for _, t := range requests {
		if now.Sub(t) < time.Hour {
			valid = append(valid, t)
		}
	}
	
	rl.requests[key] = valid
}

func (rl *RateLimiter) ResetDailyUsage() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	for key := range rl.dailyUsage {
		rl.dailyUsage[key] = 0
	}
}

type APIService struct {
	mu          sync.RWMutex
	keys        map[string]*APIKey
	keyByID     map[string]*APIKey
	rateLimiter *RateLimiter
	rpcClient   *rpc.Client
	encryption *crypto.CryptoManager
	webhookURL  string
}

func NewAPIService(rpcURL string) (*APIService, error) {
	rpcClient, err := rpc.NewClient(rpcURL)
	if err != nil {
		return nil, err
	}

	as := &APIService{
		keys:        make(map[string]*APIKey),
		keyByID:     make(map[string]*APIKey),
		rateLimiter: NewRateLimiter(),
		rpcClient:  rpcClient,
	}

	return as, nil
}

func (as *APIService) CreateKey(userID, email, name string, tier API) (*APIKey, error) {
	if _, ok := TierConfigs[tier]; !ok {
		return nil, ErrInvalidTier
	}

	key, err := crypto.GenerateToken(32)
	if err != nil {
		return nil, err
	}

	apiKey := &APIKey{
		ID:        generateID(),
		Key:       key,
		Tier:      tier,
		UserID:    userID,
		Email:    email,
		Name:     name,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata: make(map[string]interface{}),
	}

	as.mu.Lock()
	defer as.mu.Unlock()

	as.keys[key] = apiKey
	as.keyByID[apiKey.ID] = apiKey

	return apiKey, nil
}

func (as *APIService) GetKey(key string) (*APIKey, error) {
	as.mu.RLock()
	defer as.mu.RUnlock()

	apiKey, ok := as.keys[key]
	if !ok {
		return nil, ErrInvalidAPIKey
	}

	if apiKey.RevokedAt != nil {
		return nil, ErrKeyRevoked
	}

	if apiKey.ExpiresAt != nil && time.Now().After(*apiKey.ExpiresAt) {
		return nil, ErrKeyRevoked
	}

	now := time.Now()
	apiKey.LastUsedAt = &now

	return apiKey, nil
}

func (as *APIService) ValidateKey(key string) error {
	_, err := as.GetKey(key)
	return err
}

func (as *APIService) RevokeKey(keyID string) error {
	as.mu.Lock()
	defer as.mu.Unlock()

	apiKey, ok := as.keyByID[keyID]
	if !ok {
		return ErrInvalidAPIKey
	}

	now := time.Now()
	apiKey.RevokedAt = &now

	delete(as.keys, apiKey.Key)

	return nil
}

func (as *APIService) UpdateTier(keyID string, tier API) error {
	if _, ok := TierConfigs[tier]; !ok {
		return ErrInvalidTier
	}

	as.mu.Lock()
	defer as.mu.Unlock()

	apiKey, ok := as.keyByID[keyID]
	if !ok {
		return ErrInvalidAPIKey
	}

	apiKey.Tier = tier
	apiKey.UpdatedAt = time.Now()

	return nil
}

func (as *APIService) GetUsage(key string) (*UsageRecord, error) {
	apiKey, err := as.GetKey(key)
	if err != nil {
		return nil, err
	}

	config := TierConfigs[apiKey.Tier]

	return &UsageRecord{
		Timestamp: time.Now(),
		UsageCount: apiKey.UsageCount,
	}, nil
}

func (as *APIService) GetQuota(key string) (int, int, error) {
	apiKey, err := as.GetKey(key)
	if err != nil {
		return 0, 0, err
	}

	config := TierConfigs[apiKey.Tier]

	return apiKey.DailyUsage, config.DailyQuota, nil
}

func (as *APIService) ListKeys(userID string) []*APIKey {
	as.mu.RLock()
	defer as.mu.RUnlock()

	var keys []*APIKey
	for _, key := range as.keys {
		if key.UserID == userID && key.RevokedAt == nil {
			keys = append(keys, key)
		}
	}

	return keys
}

func (as *APIService) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = r.URL.Query().Get("api_key")
		}

		if key == "" {
			http.Error(w, "API key required", http.StatusUnauthorized)
			return
		}

		apiKey, err := as.GetKey(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		if err := as.rateLimiter.AllowRequest(apiKey); err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}

		ctx := context.WithValue(r.Context(), "api_key", apiKey)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (as *APIService) GetTierInfo(tier API) (TierConfig, error) {
	config, ok := TierConfigs[tier]
	if !ok {
		return TierConfig{}, ErrInvalidTier
	}
	return config, nil
}

func (as *APIService) CheckFeature(key string, feature string) error {
	apiKey, err := as.GetKey(key)
	if err != nil {
		return err
	}

	config := TierConfigs[apiKey.Tier]
	for _, f := range config.Features {
		if f == feature {
			return nil
		}
	}

	if feature == "all_features" {
		return nil
	}

	return ErrTierUpgrade
}

func generateID() string {
	return fmt.Sprintf("key_%d", time.Now().UnixNano())
}

type APIResponse struct {
	Status  string      `json:"status"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	RateLimit *RateLimitInfo `json:"rate_limit,omitempty"`
}

type RateLimitInfo struct {
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	Reset     int64  `json:"reset"`
}

func (as *APIService) BuildResponse(w http.ResponseWriter, r *http.Request, data interface{}) {
	key := r.Header.Get("X-API-Key")
	var rateInfo *RateLimitInfo

	if key != "" {
		apiKey, _ := as.GetKey(key)
		if apiKey != nil {
			config := TierConfigs[apiKey.Tier]
			rateInfo = &RateLimitInfo{
				Limit:     config.DailyQuota,
				Remaining: config.DailyQuota - apiKey.DailyUsage,
				Reset:     time.Now().Add(24 * time.Hour).Unix(),
			}
		}
	}

	resp := APIResponse{
		Status: "success",
		Data:   data,
	}

	if rateInfo != nil {
		resp.RateLimit = rateInfo
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (as *APIService) BuildErrorResponse(w http.ResponseWriter, err error) {
	resp := APIResponse{
		Status: "error",
		Error:  err.Error(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(resp)
}