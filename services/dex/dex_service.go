// Package dex provides real-time DEX data integration for TigerScan
// Supports PancakeSwap, Uniswap V2/V3 with live data feeds

package dex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/tigersmartchain/tigerscan/internal/crypto"
	"github.com/tigersmartchain/tigerscan/internal/rpc"
)

const (
	PancakeSwapFactoryV2 = "0xca143ce32fe78f1f5adf2bf3db9e1f5adf2bf3db9e1f5adf2bf3db9e1f5"
	UniswapFactoryV2     = "0x5c69bfe6f0ceda5fadf2bf3db9e1f5adf2bf3db9e1f5"
	UniswapFactoryV3     = "0x1f98431c85a02af3df5a7b4e4ff9bd5bf4e4ff9b"
	
	UpdateInterval = 15 * time.Second
	MaxPairs = 10000
)

var (
	ErrInvalidPair = errors.New("invalid trading pair")
	ErrNoLiquidity = errors.New("no liquidity")
	ErrAPIUnavailable = errors.New("DEX API unavailable")
)

type DEXType string

const (
	PancakeSwapV2 DEXType = "pancakeswap_v2"
	PancakeSwapV3 DEXType = "pancakeswap_v3"
	UniswapV2     DEXType = "uniswap_v2"
	UniswapV3     DEXType = "uniswap_v3"
)

type Token struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Decimals int    `json:"decimals"`
}

type Pair struct {
	Address    string  `json:"address"`
	Token0    Token   `json:"token0"`
	Token1    Token   `json:"token1"`
	Reserve0  string  `json:"reserve0"`
	Reserve1  string  `json:"reserve1"`
	Liquidity string  `json:"liquidity"`
}

type Pool struct {
	Address       string        `json:"address"`
	Token0       Token         `json:"token0"`
	Token1       Token         `json:"token1"`
	Fee          int           `json:"fee"`
	Liquidity    string       `json:"liquidity"`
	SqrtPriceX96 string       `json:"sqrt_price_x96"`
	Tick         int           `json:"tick"`
}

type Swap struct {
	ID          string    `json:"id"`
	Pair        string    `json:"pair"`
	Timestamp   time.Time `json:"timestamp"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	TokenIn     string    `json:"token_in"`
	TokenOut    string    `json:"token_out"`
	AmountIn    string    `json:"amount_in"`
	AmountOut   string    `json:"amount_out"`
	Sender      string    `json:"sender"`
	Recipient   string    `json:"recipient"`
	TransactionHash string `json:"transaction_hash"`
}

type PriceData struct {
	Price     string    `json:"price"`
	PriceUSD  float64   `json:"price_usd"`
	Volume24h float64   `json:"volume_24h"`
	Change24h float64   `json:"change_24h"`
	High24h   float64   `json:"high_24h"`
	Low24h   float64   `json:"low_24h"`
	UpdatedAt time.Time `json:"updated_at"`
}

type DEXService struct {
	mu          sync.RWMutex
	rpcClient   *rpc.Client
	pairs      map[string]*Pair
	pools      map[string]*Pool
	swaps      map[string][]Swap
	prices     map[string]*PriceData
	dexType    DEXType
	factory   string
	subgraph  string
	ctx      context.Context
	cancel   context.CancelFunc
	workers  int
	encryption *crypto.CryptoManager
}

func NewDEXService(rpcURL string, dexType DEXType) (*DEXService, error) {
	ctx, cancel := context.WithCancel(context.Background())
	
	rpcClient, err := rpc.NewClient(rpcURL)
	if err != nil {
		cancel()
		return nil, err
	}

	ds := &DEXService{
		rpcClient:  rpcClient,
		pairs:    make(map[string]*Pair),
		pools:    make(map[string]*Pool),
		swaps:    make(map[string][]Swap),
		prices:   make(map[string]*PriceData),
		dexType:  dexType,
		ctx:     ctx,
		cancel:  cancel,
		workers: 4,
	}

	if err := ds.initialize(); err != nil {
		cancel()
		return nil, err
	}

	go ds.startSyncLoop()
	return ds, nil
}

func (ds *DEXService) initialize() error {
	switch ds.dexType {
	case PancakeSwapV2, PancakeSwapV3:
		ds.factory = PancakeSwapFactoryV2
		ds.subgraph = "https://api.pancakeswap.com/api/v1/graphql"
	case UniswapV2:
		ds.factory = UniswapFactoryV2
		ds.subgraph = "https://api.thegraph.com/subgraphs/name/uniswap/uniswap-v2"
	case UniswapV3:
		ds.factory = UniswapFactoryV3
		ds.subgraph = "https://api.thegraph.com/subgraphs/name/uniswap/uniswap-v3"
	default:
		return fmt.Errorf("unsupported DEX type: %s", ds.dexType)
	}
	return nil
}

func (ds *DEXService) startSyncLoop() {
	ticker := time.NewTicker(UpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ds.ctx.Done():
			return
		case <-ticker.C:
			if err := ds.syncPairs(); err != nil {
				continue
			}
			if err := ds.syncPrices(); err != nil {
				continue
			}
		}
	}
}

func (ds *DEXService) syncPairs() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	pairs, err := ds.fetchPairsFromSubgraph()
	if err != nil {
		return err
	}

	for _, pair := range pairs {
		ds.pairs[pair.Address] = pair
	}

	return nil
}

func (ds *DEXService) fetchPairsFromSubgraph() ([]*Pair, error) {
	query := fmt.Sprintf(`{
		pairs(first: %d, orderBy: reserveUSD, orderDirection: desc) {
			id
			token0 { id symbol name decimals }
			token1 { id symbol name decimals }
			reserve0
			reserve1
			reserveUSD
		}
	}`, MaxPairs)

	data, err := ds.querySubgraph(query)
	if err != nil {
		return nil, err
	}

	var result struct {
		Pairs []*Pair `json:"pairs"`
	}

	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	return result.Pairs, nil
}

func (ds *DEXService) querySubgraph(query string) ([]byte, error) {
	reqBody := map[string]string{"query": query}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := ds.rpcClient.Post(ds.subgraph, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if errors, ok := result["errors"]; ok {
		return nil, fmt.Errorf("graphQL errors: %v", errors)
	}

	data, ok := result["data"].([]byte)
	if !ok {
		data, _ = json.Marshal(result["data"])
	}

	return data, nil
}

func (ds *DEXService) syncPrices() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	for addr, pair := range ds.pairs {
		price, err := ds.calculatePrice(pair)
		if err != nil {
			continue
		}
		ds.prices[addr] = price
	}

	return nil
}

func (ds *DEXService) calculatePrice(pair *Pair) (*PriceData, error) {
	reserve0 := new(big.Float)
	reserve0.SetString(pair.Reserve0)
	reserve1 := new(big.Float)
	reserve1.SetString(pair.Reserve1)

	if reserve0.Sign() == 0 {
		return nil, ErrNoLiquidity
	}

	price := new(big.Float).Quo(reserve1, reserve0)
	priceStr := price.String()

	return &PriceData{
		Price:     priceStr,
		UpdatedAt: time.Now(),
	}, nil
}

func (ds *DEXService) GetPair(address string) (*Pair, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	pair, ok := ds.pairs[address]
	return pair, ok
}

func (ds *DEXService) GetPrice(address string) (*PriceData, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	price, ok := ds.prices[address]
	return price, ok
}

func (ds *DEXService) GetTokenPrice(tokenAddr string) (*PriceData, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	for addr, price := range ds.prices {
		if addr == tokenAddr {
			return price, true
		}
	}
	return nil, false
}

func (ds *DEXService) GetTopPairs(limit int) []*Pair {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	pairs := make([]*Pair, 0, len(ds.pairs))
	for _, pair := range ds.pairs {
		pairs = append(pairs, pair)
	}

	if len(pairs) > limit {
		pairs = pairs[:limit]
	}

	return pairs
}

func (ds *DEXService) SearchPairs(query string, limit int) []*Pair {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	var results []*Pair
	for addr, pair := range ds.pairs {
		if len(results) >= limit {
			break
		}
		if contains(pair.Token0.Symbol, query) || contains(pair.Token1.Symbol, query) ||
			contains(pair.Token0.Name, query) || contains(pair.Token1.Name, query) {
			results = append(results, pair)
		}
	}

	return results
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && 
		(len(s) >= len(substr)) && 
		(s == substr || 
			(len(s) > len(substr) && (s[:len(substr)] == substr || 
				contains(s[1:], substr)))
}

func (ds *DEXService) GetSwaps(pairAddr string, limit int) []Swap {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	swaps, ok := ds.swaps[pairAddr]
	if !ok {
		return nil
	}

	start := 0
	if len(swaps) > limit {
		start = len(swaps) - limit
	}

	return swaps[start:]
}

func (ds *DEXService) GetPool(address string) (*Pool, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	pool, ok := ds.pools[address]
	return pool, ok
}

func (ds *DEXService) Close() error {
	ds.cancel()
	ds.mu.Lock()
	defer ds.mu.Unlock()

	for addr := range ds.pairs {
		delete(ds.pairs, addr)
	}
	for addr := range ds.prices {
		delete(ds.prices, addr)
	}

	return nil
}

type DEXAggregator struct {
	mu        sync.RWMutex
	services  map[DEXType]*DEXService
	rpcURL    string
	encryption *crypto.CryptoManager
}

func NewDEXAggregator(rpcURL string) (*DEXAggregator, error) {
	da := &DEXAggregator{
		services: make(map[DEXType]*DEXService),
		rpcURL:   rpcURL,
	}

	if err := da.initializeServices(); err != nil {
		return nil, err
	}

	return da, nil
}

func (da *DEXAggregator) initializeServices() error {
	dexTypes := []DEXType{PancakeSwapV2, UniswapV2, UniswapV3}

	for _, dexType := range dexTypes {
		service, err := NewDEXService(da.rpcURL, dexType)
		if err != nil {
			continue
		}
		da.services[dexType] = service
	}

	return nil
}

func (da *DEXAggregator) GetAggregatedPrice(tokenAddr string) (*PriceData, bool) {
	da.mu.RLock()
	defer da.mu.RUnlock()

	for _, service := range da.services {
		if price, ok := service.GetTokenPrice(tokenAddr); ok {
			return price, true
		}
	}

	return nil, false
}

func (da *DEXAggregator) GetAllPrices() map[string]*PriceData {
	da.mu.RLock()
	defer da.mu.RUnlock()

	prices := make(map[string]*PriceData)
	for _, service := range da.services {
		service.mu.RLock()
		for addr, price := range service.prices {
			prices[addr] = price
		}
		service.mu.RUnlock()
	}

	return prices
}

func (da *DEXAggregator) Close() error {
	da.mu.Lock()
	defer da.mu.Unlock()

	for _, service := range da.services {
		service.Close()
	}

	return nil
}

type LiquidityPool struct {
	Address        string `json:"address"`
	Token0         Token  `json:"token0"`
	Token1         Token  `json:"token1"`
	Reserve0      string `json:"reserve0"`
	Reserve1      string `json:"reserve1"`
	ReserveUSD    string `json:"reserve_usd"`
	VolumeUSD24h  string `json:"volume_usd_24h"`
	TxCount24h    string `json:"tx_count_24h"`
	Token0Price   string `json:"token0_price"`
	Token1Price  string `json:"token1_price"`
}

type TradingVolume struct {
	Timestamp     time.Time `json:"timestamp"`
	VolumeUSD    float64  `json:"volume_usd"`
	VolumeToken0 float64  `json:"volume_token0"`
	VolumeToken1 float64  `json:"volume_token1"`
	TxCount      int     `json:"tx_count"`
}

func (ds *DEXService) GetLiquidityData(pairAddr string) (*LiquidityPool, error) {
	pair, ok := ds.GetPair(pairAddr)
	if !ok {
		return nil, ErrInvalidPair
	}

	reserve0, _ := new(big.Float).SetString(pair.Reserve0)
	reserve1, _ := new(big.Float).SetString(pair.Reserve1)
	reserveUSD, _ := new(big.Float).SetString(pair.Liquidity)

	return &LiquidityPool{
		Address:     pair.Address,
		Token0:     pair.Token0,
		Token1:     pair.Token1,
		Reserve0:   pair.Reserve0,
		Reserve1:   pair.Reserve1,
		ReserveUSD: reserveUSD.String(),
	}, nil
}

func (ds *DEXService) GetVolumeHistory(pairAddr string, days int) ([]TradingVolume, error) {
	volumes := make([]TradingVolume, days)
	
	for i := 0; i < days; i++ {
		volumes[i] = TradingVolume{
			Timestamp:  time.Now().AddDate(0, 0, -i),
			VolumeUSD: 0,
			TxCount:  0,
		}
	}

	return volumes, nil
}

func (da *DEXAggregator) GetBestPrice(tokenAddr string, amount *big.Float) (string, DEXType, error) {
	da.mu.RLock()
	defer da.mu.RUnlock()

	var bestPrice string
	var bestDEX DEXType

	for dexType, service := range da.services {
		if price, ok := service.GetTokenPrice(tokenAddr); ok {
			if bestPrice == "" || price.PriceUSD < bestPrice {
				bestPrice = price.Price
				bestDEX = dexType
			}
		}
	}

	if bestPrice == "" {
		return "", "", ErrNoLiquidity
	}

	return bestPrice, bestDEX, nil
}