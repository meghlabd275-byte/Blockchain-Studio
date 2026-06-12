// Package nft provides NFT floor price and rarity services
// Implements live floor tracking and rarity algorithm

package nft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tigersmartchain/tigerscan/internal/crypto"
	"github.com/tigersmartchain/tigerscan/internal/rpc"
)

const (
	UpdateInterval = 5 * time.Minute
	MaxCollections = 10000
)

var (
	ErrInvalidCollection = errors.New("invalid collection")
	ErrNoSalesData    = errors.New("no sales data available")
	ErrRarityFailed = errors.New("rarity calculation failed")
)

type NFT struct {
	TokenID     string            `json:"token_id"`
	Address     string            `json:"address"`
	Owner       string            `json:"owner"`
	URI         string            `json:"uri"`
	Metadata    map[string]interface{} `json:"metadata"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Image       string            `json:"image"`
	Attributes []Trait            `json:"attributes"`
}

type Trait struct {
	TraitType  string `json:"trait_type"`
	TraitValue string `json:"trait_value"`
	Rarity    float64 `json:"rarity"`
}

type Collection struct {
	Address         string  `json:"address"`
	Name           string  `json:"name"`
	Symbol         string  `json:"symbol"`
	TotalSupply    string  `json:"total_supply"`
	Owner          string  `json:"owner"`
	BaseURI        string  `json:"base_uri"`
	ContractType   string  `json:"contract_type"`
	Slug           string  `json:"slug"`
	Description   string  `json:"description"`
	Image         string  `json:"image"`
	Banner        string  `json:"banner"`
	ExternalURL   string  `json:"external_url"`
	Twitter       string  `json:"twitter"`
	Discord       string  `json:"discord"`
}

type Sale struct {
	ID              string    `json:"id"`
	TokenID         string    `json:"token_id"`
	Collection      string    `json:"collection"`
	Price          string    `json:"price"`
	PriceUSD        float64   `json:"price_usd"`
	Seller         string    `json:"seller"`
	Buyer          string    `json:"buyer"`
	Timestamp      time.Time `json:"timestamp"`
	TransactionHash string   `json:"transaction_hash"`
}

type FloorPrice struct {
	Current  float64   `json:"current"`
	24hChange float64  `json:"24h_change"`
	7dChange float64   `json:"7d_change"`
	30dChange float64  `json:"30d_change"`
	UpdatedAt time.Time `json:"updated_at"`
}

type RarityRank struct {
	TokenID     string  `json:"token_id"`
	RarityScore float64 `json:"rarity_score"`
	Rank       int     `json:"rank"`
	Percentile float64 `json:"percentile"`
}

type NFTService struct {
	mu           sync.RWMutex
	rpcClient    *rpc.Client
	collections map[string]*Collection
	nfts        map[string][]*NFT
	sales       map[string][]Sale
	floorPrices map[string]*FloorPrice
	rarityCache map[string][]RarityRank
	ctx         context.Context
	cancel      context.CancelFunc
	priceFeed   PriceFeed
}

type PriceFeed interface {
	GetPrice(symbol string) (float64, error)
}

func NewNFTService(rpcURL string, pf PriceFeed) (*NFTService, error) {
	ctx, cancel := context.WithCancel(context.Background())

	rpcClient, err := rpc.NewClient(rpcURL)
	if err != nil {
		cancel()
		return nil, err
	}

	ns := &NFTService{
		rpcClient:    rpcClient,
		collections: make(map[string]*Collection),
		nfts:       make(map[string][]*NFT),
		sales:      make(map[string][]Sale),
		floorPrices: make(map[string]*FloorPrice),
		rarityCache: make(map[string][]RarityRank),
		ctx:        ctx,
		cancel:     cancel,
		priceFeed:  pf,
	}

	go ns.startSyncLoop()
	return ns, nil
}

func (ns *NFTService) startSyncLoop() {
	ticker := time.NewTicker(UpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ns.ctx.Done():
			return
		case <-ticker.C:
			ns.syncFloorPrices()
			ns.calculateRarity()
		}
	}
}

func (ns *NFTService) syncFloorPrices() {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	for addr := range ns.collections {
		sales := ns.sales[addr]
		if len(sales) == 0 {
			continue
		}

		current := calculateFloor(sales)
		
		var dayChange, weekChange, monthChange float64
		
		now := time.Now()
		dayAgo := now.AddDate(0, 0, -1)
		weekAgo := now.AddDate(0, 0, -7)
		monthAgo := now.AddDate(0, 0, -30)

		var daySales, weekSales, monthSales []Sale
		for _, sale := range sales {
			if sale.Timestamp.After(dayAgo) {
				daySales = append(daySales, sale)
			}
			if sale.Timestamp.After(weekAgo) {
				weekSales = append(weekSales, sale)
			}
			if sale.Timestamp.After(monthAgo) {
				monthSales = append(monthSales, sale)
			}
		}

		dayFloor := calculateFloor(daySales)
		weekFloor := calculateFloor(weekSales)
		monthFloor := calculateFloor(monthSales)

		if current > 0 && dayFloor > 0 {
			dayChange = ((current - dayFloor) / dayFloor) * 100
		}
		if current > 0 && weekFloor > 0 {
			weekChange = ((current - weekFloor) / weekFloor) * 100
		}
		if current > 0 && monthFloor > 0 {
			monthChange = ((current - monthFloor) / monthFloor) * 100
		}

		ns.floorPrices[addr] = &FloorPrice{
			Current:   current,
			24hChange: dayChange,
			7dChange: weekChange,
			30dChange: monthChange,
			UpdatedAt: time.Now(),
		}
	}
}

func calculateFloor(sales []Sale) float64 {
	if len(sales) == 0 {
		return 0
	}

	minPrice := math.MaxFloat64
	for _, sale := range sales {
		if sale.PriceUSD < minPrice && sale.PriceUSD > 0 {
			minPrice = sale.PriceUSD
		}
	}

	if minPrice == math.MaxFloat64 {
		return 0
	}

	return minPrice
}

func (ns *NFTService) calculateRarity() {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	for addr, nfts := range ns.nfts {
		if len(nfts) == 0 {
			continue
		}

		ranks := calculateRarityRanks(nfts)
		ns.rarityCache[addr] = ranks
	}
}

func calculateRarityRanks(nfts []*NFT) []RarityRank {
	traitCounts := make(map[string]map[string]int)
	totalNFTs := float64(len(nfts))

	for _, nft := range nfts {
		for _, attr := range nft.Attributes {
			if traitCounts[attr.TraitType] == nil {
				traitCounts[attr.TraitType] = make(map[string]int)
			}
			traitCounts[attr.TraitType][attr.TraitValue]++
		}
	}

	type scoredNFT struct {
		nft      *NFT
		score   float64
	}

	var scored []scoredNFT

	for _, nft := range nfts {
		score := 1.0

		for _, attr := range nft.Attributes {
			count := traitCounts[attr.TraitType][attr.TraitValue]
			if count > 0 {
				rarity := totalNFTs / float64(count)
				score *= rarity
			}
		}

		score = math.Log(score)
		scored = append(scored, scoredNFT{nft: nft, score: score})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	ranks := make([]RarityRank, len(scored))
	for i, s := range scored {
		percentile := float64(i) / float64(len(scored)) * 100
		ranks = append(ranks, RarityRank{
			TokenID:     s.nft.TokenID,
			RarityScore: s.score,
			Rank:       i + 1,
			Percentile: percentile,
		})
	}

	return ranks
}

func (ns *NFTService) GetCollection(address string) (*Collection, bool) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	col, ok := ns.collections[address]
	return col, ok
}

func (ns *NFTService) GetFloorPrice(address string) (*FloorPrice, bool) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	fp, ok := ns.floorPrices[address]
	return fp, ok
}

func (ns *NFTService) GetRarityRank(collection, tokenID string) (*RarityRank, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	ranks, ok := ns.rarityCache[collection]
	if !ok {
		return nil, ErrRarityFailed
	}

	for _, rank := range ranks {
		if rank.TokenID == tokenID {
			return &rank, nil
		}
	}

	return nil, ErrRarityFailed
}

func (ns *NFTService) GetTopRarest(collection string, limit int) []RarityRank {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	ranks, ok := ns.rarityCache[collection]
	if !ok {
		return nil
	}

	if limit > len(ranks) {
		limit = len(ranks)
	}

	return ranks[:limit]
}

func (ns *NFTService) AddSale(sale Sale) {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	ns.sales[sale.Collection] = append(ns.sales[sale.Collection], sale)
	
	if len(ns.sales[sale.Collection]) > 1000 {
		ns.sales[sale.Collection] = ns.sales[sale.Collection][len(ns.sales[sale.Collection])-1000:]
	}
}

func (ns *NFTService) GetSales(collection string, limit int) []Sale {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	sales, ok := ns.sales[collection]
	if !ok {
		return nil
	}

	start := 0
	if len(sales) > limit {
		start = len(sales) - limit
	}

	return sales[start:]
}

func (ns *NFTService) GetVolume(collection string) (float64, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	sales, ok := ns.sales[collection]
	if !ok {
		return 0, ErrNoSalesData
	}

	now := time.Now()
	monthAgo := now.AddDate(0, 0, -30)

	var volume float64
	for _, sale := range sales {
		if sale.Timestamp.After(monthAgo) {
			volume += sale.PriceUSD
		}
	}

	return volume, nil
}

func (ns *NFTService) GetAveragePrice(collection string) (float64, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	sales := ns.sales[collection]
	if len(sales) == 0 {
		return 0, ErrNoSalesData
	}

	var total float64
	count := 0

	for i := len(sales) - 1; i >= 0 && count < 100; i-- {
		if sales[i].PriceUSD > 0 {
			total += sales[i].PriceUSD
			count++
		}
	}

	if count == 0 {
		return 0, ErrNoSalesData
	}

	return total / float64(count), nil
}

func (ns *NFTService) GetHolderCount(collection string) int {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	nfts := ns.nfts[collection]
	owners := make(map[string]bool)

	for _, nft := range nfts {
		owners[nft.Owner] = true
	}

	return len(owners)
}

func (ns *NFTService) GetTraitStats(collection string) map[string]map[string]float64 {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	nfts := ns.nfts[collection]
	if len(nfts) == 0 {
		return nil
	}

	traitStats := make(map[string]map[string]float64)
	traitCounts := make(map[string]map[string]int)
	totalNFTs := float64(len(nfts))

	for _, nft := range nfts {
		for _, attr := range nft.Attributes {
			if traitCounts[attr.TraitType] == nil {
				traitCounts[attr.TraitType] = make(map[string]int)
			}
			traitCounts[attr.TraitType][attr.TraitValue]++
		}
	}

	for traitType, values := range traitCounts {
		traitStats[traitType] = make(map[string]float64)
		for value, count := range values {
			rarity := (float64(count) / totalNFTs) * 100
			traitStats[traitType][value] = rarity
		}
	}

	return traitStats
}

func (ns *NFTService) ParseMetadata(nft *NFT) error {
	if nft.URI == "" {
		return nil
	}

	var metadata map[string]interface{}
	if err := json.Unmarshal([]byte(nft.URI), &metadata); err != nil {
		return err
	}

	nft.Metadata = metadata

	if name, ok := metadata["name"].(string); ok {
		nft.Name = name
	}
	if desc, ok := metadata["description"].(string); ok {
		nft.Description = desc
	}
	if image, ok := metadata["image"].(string); ok {
		nft.Image = image
	}

	if attrs, ok := metadata["attributes"].([]interface{}); ok {
		for _, a := range attrs {
			if attrMap, ok := a.(map[string]interface{}); ok {
				traitType, _ := attrMap["trait_type"].(string)
				traitValue, _ := attrMap["value"].(string)

				nft.Attributes = append(nft.Attributes, Trait{
					TraitType:  traitType,
					TraitValue: traitValue,
				})
			}
		}
	}

	return nil
}

func (ns *NFTService) Close() error {
	ns.cancel()
	ns.mu.Lock()
	defer ns.mu.Unlock()

	ns.collections = nil
	ns.nfts = nil
	ns.sales = nil

	return nil
}

type NFTResponse struct {
	Status string      `json:"status"`
	Data  interface{} `json:"data,omitempty"`
	Error string    `json:"error,omitempty"`
}

func (ns *NFTService) ToFloorPriceJSON(address string) ([]byte, error) {
	fp, ok := ns.GetFloorPrice(address)
	if !ok {
		return nil, ErrNoSalesData
	}

	return json.Marshal(fp)
}

func ParseTokenID(tokenID string) (*big.Int, error) {
	id, ok := new(big.Int).SetString(tokenID, 0)
	if !ok {
		return nil, fmt.Errorf("invalid token ID: %s", tokenID)
	}
	return id, nil
}

func FormatPrice(priceWei *big.Int) string {
	priceEth := new(big.Float).SetInt(priceWei)
	priceEth = new(big.Float).Quo(priceEth, new(big.Float).SetInt(big.NewInt(1e18)))
	return priceEth.String()
}

func ParsePrice(priceStr string) (*big.Int, error) {
	priceF, _, err := big.ParseFloat(priceStr, 10, 0, big.ToNearestEven)
	if err != nil {
		return nil, err
	}

	priceWei := new(big.Int).Mul(priceF, new(big.Int).SetInt(big.NewInt(1e18)))
	return priceWei, nil
}

func (ns *NFTService) SearchCollections(query string, limit int) []*Collection {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	var results []*Collection
	for addr, col := range ns.collections {
		if len(results) >= limit {
			break
		}

		if contains(col.Name, query) || contains(col.Symbol, query) {
			results = append(results, col)
		}
	}

	return results
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && 
		(s == substr || 
			(len(s) > len(substr) && 
				(findSubstring(s, substr))))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}