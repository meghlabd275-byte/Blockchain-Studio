// Package webhook provides webhook event system for TigerScan
// Implements event triggers and notifications

package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/tigersmartchain/tigerscan/internal/crypto"
)

const (
	MaxWebhooks = 1000
	MaxRetries = 3
	RetryDelay = 5 * time.Second
)

var (
	ErrInvalidURL      = errors.New("invalid webhook URL")
	ErrInvalidSecret  = errors.New("invalid webhook secret")
	ErrWebhookNotFound = errors.New("webhook not found")
	ErrDeliveryFailed = errors.New("webhook delivery failed")
)

type EventType string

const (
	EventBlock          EventType = "block"
	EventTransaction   EventType = "transaction"
	EventTokenTransfer EventType = "token_transfer"
	EventNFTransfer    EventType = "nft_transfer"
	EventTokenApproval EventType = "token_approval"
	EventGasPrice      EventType = "gas_price"
	EventNewContract  EventType = "new_contract"
)

type Webhook struct {
	ID          string     `json:"id"`
	URL         string     `json:"url"`
	Secret     string     `json:"secret"`
	EventTypes []EventType `json:"event_types"`
	Active     bool       `json:"active"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastTrigger *time.Time `json:"last_trigger,omitempty"`
	TriggerCount int      `json:"trigger_count"`
	FailCount   int       `json:"fail_count"`
	Filters    FilterRule `json:"filters"`
}

type FilterRule struct {
	Addresses []string `json:"addresses,omitempty"`
	MinValue  string   `json:"min_value,omitempty"`
	MaxValue  string   `json:"max_value,omitempty"`
	Tokens    []string `json:"tokens,omitempty"`
}

type WebhookEvent struct {
	ID        string    `json:"id"`
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
}

type Delivery struct {
	ID          string    `json:"id"`
	WebhookID  string    `json:"webhook_id"`
	EventID   string    `json:"event_id"`
	URL       string    `json:"url"`
	Timestamp time.Time `json:"timestamp"`
	StatusCode int     `json:"status_code"`
	Success   bool     `json:"success"`
	Error     string   `json:"error,omitempty"`
	Duration  int64    `json:"duration_ms"`
	Retries   int      `json:"retries"`
}

type WebhookService struct {
	mu        sync.RWMutex
	webhooks map[string]*Webhook
	events   []WebhookEvent
	deliveries []Delivery
	httpClient *http.Client
	encryption *crypto.CryptoManager
	ctx       context.Context
	cancel    context.CancelFunc
}

func NewWebhookService() (*WebhookService, error) {
	ctx, cancel := context.WithCancel(context.Background())

	ws := &WebhookService{
		webhooks:   make(map[string]*Webhook),
		events:     make([]WebhookEvent, 0, 10000),
		deliveries: make([]Delivery, 0, 10000),
		httpClient: &http.Client{Timeout: 30 * time.Second},
		ctx:       ctx,
		cancel:    cancel,
	}

	go ws.startEventProcessor()
	return ws, nil
}

func (ws *WebhookService) CreateWebhook(url string, secret string, eventTypes []EventType, filters FilterRule) (*Webhook, error) {
	if !isValidURL(url) {
		return nil, ErrInvalidURL
	}

	if len(ws.webhooks) >= MaxWebhooks {
		return nil, fmt.Errorf("max webhooks reached")
	}

	webhook := &Webhook{
		ID:         generateID(),
		URL:        url,
		Secret:    secret,
		EventTypes: eventTypes,
		Active:    true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Filters:   filters,
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	ws.webhooks[webhook.ID] = webhook

	return webhook, nil
}

func (ws *WebhookService) GetWebhook(id string) (*Webhook, bool) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	webhook, ok := ws.webhooks[id]
	return webhook, ok
}

func (ws *WebhookService) UpdateWebhook(id string, url string, eventTypes []EventType, active bool) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	webhook, ok := ws.webhooks[id]
	if !ok {
		return ErrWebhookNotFound
	}

	if url != "" {
		if !isValidURL(url) {
			return ErrInvalidURL
		}
		webhook.URL = url
	}

	if len(eventTypes) > 0 {
		webhook.EventTypes = eventTypes
	}

	webhook.Active = active
	webhook.UpdatedAt = time.Now()

	return nil
}

func (ws *WebhookService) DeleteWebhook(id string) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if _, ok := ws.webhooks[id]; !ok {
		return ErrWebhookNotFound
	}

	delete(ws.webhooks, id)
	return nil
}

func (ws *WebhookService) ListWebhooks() []*Webhook {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	webhooks := make([]*Webhook, 0, len(ws.webhooks))
	for _, wh := range ws.webhooks {
		webhooks = append(webhooks, wh)
	}

	return webhooks
}

func (ws *WebhookService) Trigger(eventType EventType, data map[string]interface{}) {
	event := WebhookEvent{
		ID:        generateID(),
		Type:      eventType,
		Timestamp: time.Now(),
		Data:      data,
	}

	ws.mu.Lock()
	ws.events = append(ws.events, event)
	if len(ws.events) > 10000 {
		ws.events = ws.events[len(ws.events)-10000:]
	}
	ws.mu.Unlock()
}

func (ws *WebhookService) startEventProcessor() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ws.ctx.Done():
			return
		case <-ticker.C:
			ws.processEvents()
		}
	}
}

func (ws *WebhookService) processEvents() {
	ws.mu.RLock()
	if len(ws.events) == 0 {
		ws.mu.RUnlock()
		return
	}

	event := ws.events[0]
	ws.events = ws.events[1:]
	ws.mu.RUnlock()

	for _, webhook := range ws.webhooks {
		if !webhook.Active {
			continue
		}

		if !containsEventType(webhook.EventTypes, event.Type) {
			continue
		}

		if !ws.matchFilters(&webhook.Filters, event.Data) {
			continue
		}

		go ws.deliverEvent(webhook, &event)
	}
}

func (ws *WebhookService) deliverEvent(webhook *Webhook, event *WebhookEvent) {
	payload, _ := json.Marshal(event)

	startTime := time.Now()
	success := false
	var statusCode int
	var errMsg string

	for attempt := 0; attempt < MaxRetries; attempt++ {
		req, err := http.NewRequest("POST", webhook.URL, bytes.NewReader(payload))
		if err != nil {
			errMsg = err.Error()
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Webhook-Event", string(event.Type))
		req.Header.Set("X-Webhook-ID", webhook.ID)

		if webhook.Secret != "" {
			signature := generateSignature(payload, webhook.Secret)
			req.Header.Set("X-Signature", signature)
		}

		resp, err := ws.httpClient.Do(req)
		if err != nil {
			errMsg = err.Error()
			time.Sleep(RetryDelay)
			continue
		}

		statusCode = resp.StatusCode
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			success = true
			break
		}

		time.Sleep(RetryDelay)
	}

	duration := time.Since(startTime).Milliseconds()

	delivery := Delivery{
		ID:         generateID(),
		WebhookID: webhook.ID,
		EventID:   event.ID,
		URL:       webhook.URL,
		Timestamp: startTime,
		StatusCode: statusCode,
		Success:   success,
		Error:     errMsg,
		Duration:  duration,
		Retries:   MaxRetries,
	}

	ws.mu.Lock()
	ws.deliveries = append(ws.deliveries, delivery)
	if len(ws.deliveries) > 10000 {
		ws.deliveries = ws.deliveries[len(ws.deliveries)-10000:]
	}
	ws.mu.Unlock()

	webhook.TriggerCount++
	if !success {
		webhook.FailCount++
	}
	now := time.Now()
	webhook.LastTrigger = &now
}

func (ws *WebhookService) matchFilters(filters *FilterRule, data map[string]interface{}) bool {
	if len(filters.Addresses) > 0 {
		addr, ok := data["address"].(string)
		if !ok {
			return false
		}
		found := false
		for _, a := range filters.Addresses {
			if a == addr {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func generateSignature(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func generateID() string {
	return fmt.Sprintf("wh_%d", time.Now().UnixNano())
}

func isValidURL(url string) bool {
	if len(url) < 10 {
		return false
	}
	return url[:4] == "http" || url[:5] == "https"
}

func containsEventType(types []EventType, eventType EventType) bool {
	for _, t := range types {
		if t == eventType {
			return true
		}
	}
	return false
}

func (ws *WebhookService) GetDeliveries(webhookID string, limit int) []Delivery {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	var results []Delivery
	for i := len(ws.deliveries) - 1; i >= 0 && len(results) < limit; i-- {
		if ws.deliveries[i].WebhookID == webhookID {
			results = append(results, ws.deliveries[i])
		}
	}

	return results
}

func (ws *WebhookService) GetStats() map[string]interface{} {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	var totalDeliveries, successful, failed int
	for _, d := range ws.deliveries {
		totalDeliveries++
		if d.Success {
			successful++
		} else {
			failed++
		}
	}

	return map[string]interface{}{
		"total_webhooks":  len(ws.webhooks),
		"total_events":   len(ws.events),
		"total_deliveries": totalDeliveries,
		"successful":    successful,
		"failed":        failed,
	}
}

func (ws *WebhookService) Close() error {
	ws.cancel()
	ws.mu.Lock()
	defer ws.mu.Unlock()

	ws.webhooks = nil
	ws.events = nil
	ws.deliveries = nil

	return nil
}

var bytes = struct {
	NewReader func([]byte) interface{} {
		return nil
	}
}{
	NewReader: func(b []byte) interface{} { return b },
}