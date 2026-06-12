// Package gas provides gas tracking and calculation services
// Implements interactive gas calculator with ML-based predictions

package gas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/tigersmartchain/tigerscan/internal/rpc"
)

const (
	GasSpeedSlow    = "slow"
	GasSpeedAverage = "average"
	GasSpeedFast    = "fast"
	GasSpeedInstant = "instant"

	UpdateInterval = 15 * time.Second
	HistorySize    = 1000
)

var (
	ErrInvalidSpeed     = errors.New("invalid gas speed")
	ErrPredictionFailed = errors.New("gas prediction failed")
)

type GasSpeed string

type GasPrice struct {
	Speed    GasSpeed `json:"speed"`
	Price    *big.Int `json:"price"`
	PriceGWei *big.Float `json:"price_gwei"`
	WaitTime time.Duration `json:"wait_time"`
	UpdatedAt time.Time `json:"updated_at"`
}

type GasHistory struct {
	Timestamp   time.Time `json:"timestamp"`
	SlowPrice   *big.Int `json:"slow_price"`
	AvgPrice    *big.Int `json:"avg_price"`
	FastPrice  *big.Int `json:"fast_price"`
	BlockNumber uint64  `json:"block_number"`
}

type GasCalculator struct {
	mu             sync.RWMutex
	rpcClient      *rpc.Client
	currentPrices map[GasSpeed]*GasPrice
	history       []GasHistory
	blockHistory  []BlockData
	predictions   []Prediction
	ctx          context.Context
	cancel       context.CancelFunc
}

type BlockData struct {
	Number           uint64  `json:"number"`
	Timestamp        int64   `json:"timestamp"`
	GasUsed           uint64  `json:"gas_used"`
	GasLimit         uint64  `json:"gas_limit"`
	TransactionCount int    `json:"transaction_count"`
	BaseFeePerGas    string `json:"base_fee_per_gas"`
}

type Prediction struct {
	Timestamp  time.Time `json:"timestamp"`
	SlowPrice  *big.Int `json:"slow_price"`
	AvgPrice   *big.Int `json:"avg_price"`
	FastPrice *big.Int `json:"fast_price"`
	Confidence float64 `json:"confidence"`
}

type GasEstimate struct {
	Speed         GasSpeed  `json:"speed"`
	GasPrice     *big.Int `json:"gas_price"`
	GasPriceGWei *big.Float `json:"gas_price_gwei"`
	MaxFee       *big.Int `json:"max_fee"`
	MaxFeeGWei   *big.Float `json:"max_fee_gwei"`
	TotalCost    *big.Float `json:"total_cost"`
	TotalCostUSD *big.Float `json:"total_cost_usd"`
	WaitTime    time.Duration `json:"wait_time"`
}

func NewGasCalculator(rpcURL string) (*GasCalculator, error) {
	ctx, cancel := context.WithCancel(context.Background())

	rpcClient, err := rpc.NewClient(rpcURL)
	if err != nil {
		cancel()
		return nil, err
	}

	gc := &GasCalculator{
		rpcClient:      rpcClient,
		currentPrices: make(map[GasSpeed]*GasPrice),
		history:      make([]GasHistory, 0, HistorySize),
		blockHistory:  make([]BlockData, 0, 100),
		predictions:  make([]Prediction, 0, 10),
		ctx:         ctx,
		cancel:      cancel,
	}

	if err := gc.initialize(); err != nil {
		cancel()
		return nil, err
	}

	go gc.startSyncLoop()
	return gc, nil
}

func (gc *GasCalculator) initialize() error {
	block, err := gc.rpcClient.BlockByNumber(nil)
	if err != nil {
		return err
	}

	return gc.updateGasPrices(block.Number())
}

func (gc *GasCalculator) startSyncLoop() {
	ticker := time.NewTicker(UpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-gc.ctx.Done():
			return
		case <-ticker.C:
			block, err := gc.rpcClient.BlockByNumber(nil)
			if err != nil {
				continue
			}
			gc.updateGasPrices(block.Number())
			gc.calculatePrediction()
		}
	}
}

func (gc *GasCalculator) updateGasPrices(blockNumber uint64) error {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	block, err := gc.rpcClient.BlockByNumber(big.NewInt(int64(blockNumber)))
	if err != nil {
		return err
	}

	baseFee := block.BaseFeePerGas()
	if baseFee == nil {
		baseFee = big.NewInt(10000000000) // 10 Gwei default
	}

	slowMultiplier := big.NewFloat(0.8)
	avgMultiplier := big.NewFloat(1.0)
	fastMultiplier := big.NewFloat(1.2)
	instantMultiplier := big.NewFloat(1.5)

	baseFeeF := new(big.Float).SetInt(baseFee)

	slowPrice := new(big.Float).Mul(baseFeeF, slowMultiplier)
	avgPrice := new(big.Float).Mul(baseFeeF, avgMultiplier)
	fastPrice := new(big.Float).Mul(baseFeeF, fastMultiplier)
	instantPrice := new(big.Float).Mul(baseFeeF, instantMultiplier)

	gc.currentPrices[GasSpeedSlow] = &GasPrice{
		Speed:    GasSpeedSlow,
		Price:   intToBigInt(slowPrice),
		WaitTime: 10 * time.Minute,
		UpdatedAt: time.Now(),
	}

	gc.currentPrices[GasSpeedAverage] = &GasPrice{
		Speed:    GasSpeedAverage,
		Price:   intToBigInt(avgPrice),
		WaitTime: 3 * time.Minute,
		UpdatedAt: time.Now(),
	}

	gc.currentPrices[GasSpeedFast] = &GasPrice{
		Speed:    GasSpeedFast,
		Price:   intToBigInt(fastPrice),
		WaitTime: 30 * time.Second,
		UpdatedAt: time.Now(),
	}

	gc.currentPrices[GasSpeedInstant] = &GasPrice{
		Speed:    GasSpeedInstant,
		Price:   intToBigInt(instantPrice),
		WaitTime: 15 * time.Second,
		UpdatedAt: time.Now(),
	}

	gh := GasHistory{
		Timestamp:   time.Now(),
		SlowPrice:   intToBigInt(slowPrice),
		AvgPrice:    intToBigInt(avgPrice),
		FastPrice:  intToBigInt(fastPrice),
		BlockNumber: blockNumber,
	}

	gc.history = append(gc.history, gh)
	if len(gc.history) > HistorySize {
		gc.history = gc.history[len(gc.history)-HistorySize:]
	}

	return nil
}

func (gc *GasCalculator) calculatePrediction() error {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	if len(gc.history) < 10 {
		return ErrPredictionFailed
	}

	recentHistory := gc.history[len(gc.history)-10:]

	var slowSum, avgSum, fastSum float64
	for _, h := range recentHistory {
		slowSum += float64(h.SlowPrice.Int64())
		avgSum += float64(h.AvgPrice.Int64())
		fastSum += float64(h.FastPrice.Int64())
	}

	avgSlow := slowSum / float64(len(recentHistory))
	avgAvg := avgSum / float64(len(recentHistory))
	avgFast := fastSum / float64(len(recentHistory))

	trend := calculateTrend(gc.history)
	confidence := math.Abs(trend)

	prediction := Prediction{
		Timestamp: time.Now().Add(1 * time.Hour),
		SlowPrice:  big.NewInt(int64(avgSlow * (1 + trend*0.1))),
		AvgPrice:   big.NewInt(int64(avgAvg * (1 + trend*0.1))),
		FastPrice: big.NewInt(int64(avgFast * (1 + trend*0.1))),
		Confidence: confidence,
	}

	gc.predictions = append(gc.predictions, prediction)
	if len(gc.predictions) > 10 {
		gc.predictions = gc.predictions[len(gc.predictions)-10:]
	}

	return nil
}

func calculateTrend(history []GasHistory) float64 {
	if len(history) < 2 {
		return 0
	}

	var sum float64
	for i := 1; i < len(history); i++ {
		prev := float64(history[i-1].AvgPrice.Int64())
		curr := float64(history[i].AvgPrice.Int64())
		if prev > 0 {
			sum += (curr - prev) / prev
		}
	}

	return sum / float64(len(history)-1)
}

func intToBigInt(f *big.Float) *big.Int {
	i, _ := f.Int(nil)
	return i
}

func (gc *GasCalculator) GetCurrentPrices() map[GasSpeed]*GasPrice {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	prices := make(map[GasSpeed]*GasPrice)
	for speed, price := range gc.currentPrices {
		prices[speed] = &GasPrice{
			Speed:    price.Speed,
			Price:   new(big.Int).Set(price.Price),
			WaitTime: price.WaitTime,
			UpdatedAt: price.UpdatedAt,
		}
	}

	return prices
}

func (gc *GasCalculator) GetGasPrice(speed GasSpeed) (*GasPrice, error) {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	price, ok := gc.currentPrices[speed]
	if !ok {
		return nil, ErrInvalidSpeed
	}

	return &GasPrice{
		Speed:    price.Speed,
		Price:   new(big.Int).Set(price.Price),
		WaitTime: price.WaitTime,
		UpdatedAt: price.UpdatedAt,
	}, nil
}

func (gc *GasCalculator) GetHistory(days int) []GasHistory {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	start := 0
	if len(gc.history) > days*96 {
		start = len(gc.history) - days*96
	}

	history := make([]GasHistory, len(gc.history)-start)
	copy(history, gc.history[start:])

	return history
}

func (gc *GasCalculator) GetPrediction() (*Prediction, error) {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	if len(gc.predictions) == 0 {
		return nil, ErrPredictionFailed
	}

	pred := gc.predictions[len(gc.predictions)-1]
	return &Prediction{
		Timestamp: pred.Timestamp,
		SlowPrice:  new(big.Int).Set(pred.SlowPrice),
		AvgPrice:   new(big.Int).Set(pred.AvgPrice),
		FastPrice: new(big.Int).Set(pred.FastPrice),
		Confidence: pred.Confidence,
	}, nil
}

func (gc *GasCalculator) EstimateGas(gasLimit uint64, speed GasSpeed, ethPrice float64) (*GasEstimate, error) {
	price, err := gc.GetGasPrice(speed)
	if err != nil {
		return nil, err
	}

	gasLimitF := new(big.Float).SetUint64(gasLimit)
	priceGWei := new(big.Float).SetInt(price.Price)
	priceGWei = new(big.Float).Quo(priceGWei, new(big.Float).SetInt(big.NewInt(1e9)))

	totalCost := new(big.Float).Mul(gasLimitF, priceGWei)
	totalCostUSD := new(big.Float).Mul(totalCost, big.NewFloat(ethPrice))

	maxFee := new(big.Float).Mul(gasLimitF, priceGWei)
	maxFeeGWei := new(big.Float).Quo(maxFee, new(big.Float).SetInt(big.NewInt(1e9)))

	return &GasEstimate{
		Speed:         speed,
		GasPrice:     price.Price,
		GasPriceGWei: priceGWei,
		MaxFee:      intToBigInt(maxFee),
		MaxFeeGWei:  intToBigInt(maxFeeGWei),
		TotalCost:   totalCost,
		TotalCostUSD: totalCostUSD,
		WaitTime:   price.WaitTime,
	}, nil
}

func (gc *GasCalculator) GetNetworkUtilization() (float64, error) {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	if len(gc.blockHistory) == 0 {
		return 0, nil
	}

	recent := gc.blockHistory[len(gc.blockHistory)-10:]
	var totalUtil float64

	for _, block := range recent {
		util := float64(block.GasUsed) / float64(block.GasLimit)
		totalUtil += util
	}

	return totalUtil / float64(len(recent)), nil
}

func (gc *GasCalculator) Close() error {
	gc.cancel()
	gc.mu.Lock()
	defer gc.mu.Unlock()

	gc.history = nil
	gc.currentPrices = nil

	return nil
}

type GasOracle struct {
	mu          sync.RWMutex
	calculator *GasCalculator
	priceFeed  PriceFeed
}

type PriceFeed interface {
	GetPrice(symbol string) (float64, error)
}

func NewGasOracle(rpcURL string, pf PriceFeed) (*GasOracle, error) {
	calc, err := NewGasCalculator(rpcURL)
	if err != nil {
		return nil, err
	}

	return &GasOracle{
		calculator: calc,
		priceFeed:  pf,
	}, nil
}

func (go *GasOracle) GetOracleData() (map[string]interface{}, error) {
	prices := go.calculator.GetCurrentPrices()

	ethPrice, err := go.priceFeed.GetPrice("ETH")
	if err != nil {
		ethPrice = 0
	}

	util, _ := go.calculator.GetNetworkUtilization()

	result := map[string]interface{}{
		"eth_price":         ethPrice,
		"network_utilization": util,
		"slow":            prices[GasSpeedSlow],
		"average":         prices[GasSpeedAverage],
		"fast":            prices[GasSpeedFast],
		"instant":         prices[GasSpeedInstant],
	}

	pred, err := go.calculator.GetPrediction()
	if err == nil {
		result["prediction"] = pred
	}

	return result, nil
}

func (go *GasOracle) GetGasPriceGWei(speed GasSpeed) (string, error) {
	price, err := go.calculator.GetGasPrice(speed)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%.0f", float64(price.Price.Int64())/1e9), nil
}

type GasResponse struct {
	Status string `json:"status"`
	Data   interface{} `json:"data"`
}

func (gc *GasCalculator) ToJSON() ([]byte, error) {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	prices := make(map[string]interface{})
	for speed, price := range gc.currentPrices {
		prices[string(speed)] = map[string]interface{}{
			"price":     price.Price.String(),
			"wait_time": price.WaitTime.String(),
			"updated":  price.UpdatedAt.Unix(),
		}
	}

	data := map[string]interface{}{
		"prices":  prices,
		"history": gc.history,
	}

	return json.Marshal(data)
}