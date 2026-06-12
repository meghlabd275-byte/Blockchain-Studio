// Package rpc provides RPC client functionality for TigerScan

package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTimeout = 30 * time.Second
	MaxBatchSize = 100
)

var (
	ErrConnectionFailed = errors.New("RPC connection failed")
	ErrInvalidRequest  = errors.New("invalid RPC request")
	ErrInvalidResponse = errors.New("invalid RPC response")
)

type Client struct {
	url      string
	httpClient *http.Client
	mu       sync.RWMutex
	stats    RPCStats
}

type RPCStats struct {
	Requests   int64
	Errors    int64
	Latency   time.Duration
	LastError error
}

type Block struct {
	Number           string   `json:"number"`
	Hash             string   `json:"hash"`
	ParentHash       string   `json:"parentHash"`
	Timestamp        string   `json:"timestamp"`
	Difficulty       string   `json:"difficulty"`
	GasLimit         string   `json:"gasLimit"`
	GasUsed          string   `json:"gasUsed"`
	Transactions    []string `json:"transactions"`
	BaseFeePerGas    *string  `json:"baseFeePerGas,omitempty"`
	Nonce            *string  `json:"nonce,omitempty"`
}

type Transaction struct {
	Hash       string `json:"hash"`
	From       string `json:"from"`
	To         string `json:"to"`
	Value      string `json:"value"`
	Gas        string `json:"gas"`
	GasPrice   string `json:"gasPrice"`
	Input      string `json:"input"`
	Nonce      string `json:"nonce"`
	BlockHash  string `json:"blockHash"`
	BlockNumber string `json:"blockNumber"`
	Status     string `json:"status"`
}

type Log struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
	Index   uint     `json:"logIndex"`
}

type Receipt struct {
	TransactionHash  string `json:"transactionHash"`
	BlockNumber      string `json:"blockNumber"`
	Status          string `json:"status"`
	GasUsed         string `json:"gasUsed"`
	Logs            []Log  `json:"logs"`
}

func NewClient(url string) (*Client, error) {
	if url == "" {
		return nil, ErrInvalidRequest
	}

	client := &http.Client{
		Timeout: DefaultTimeout,
	}

	return &Client{
		url:       url,
		httpClient: client,
	}, nil
}

func (c *Client) BlockByNumber(blockNum *big.Int) (*Block, error) {
	var blockNumStr string
	if blockNum == nil {
		blockNumStr = "latest"
	} else {
		blockNumStr = fmt.Sprintf("0x%x", blockNum.Int64())
	}

	params := []string{blockNumStr}
	result, err := c.Call("eth_getBlockByNumber", params)
	if err != nil {
		return nil, err
	}

	var block Block
	if err := json.Unmarshal(result, &block); err != nil {
		return nil, err
	}

	return &block, nil
}

func (c *Client) BlockByHash(hash string) (*Block, error) {
	params := []string{hash, true}
	result, err := c.Call("eth_getBlockByHash", params)
	if err != nil {
		return nil, err
	}

	var block Block
	if err := json.Unmarshal(result, &block); err != nil {
		return nil, err
	}

	return &block, nil
}

func (c *Client) TransactionByHash(hash string) (*Transaction, bool, error) {
	params := []string{hash, true}
	result, err := c.Call("eth_getTransactionByHash", params)
	if err != nil {
		return nil, false, err
	}

	if string(result) == "null" {
		return nil, false, nil
	}

	var tx Transaction
	if err := json.Unmarshal(result, &tx); err != nil {
		return nil, false, err
	}

	return &tx, true, nil
}

func (c *Client) TransactionReceipt(hash string) (*Receipt, error) {
	params := []string{hash}
	result, err := c.Call("eth_getTransactionReceipt", params)
	if err != nil {
		return nil, err
	}

	var receipt Receipt
	if err := json.Unmarshal(result, &receipt); err != nil {
		return nil, err
	}

	return &receipt, nil
}

func (c *Client) GetBalance(address string, blockNum *big.Int) (*big.Int, error) {
	var blockNumStr string
	if blockNum == nil {
		blockNumStr = "latest"
	} else {
		blockNumStr = fmt.Sprintf("0x%x", blockNum.Int64())
	}

	params := []string{address, blockNumStr}
	result, err := c.Call("eth_getBalance", params)
	if err != nil {
		return nil, err
	}

	var balance string
	if err := json.Unmarshal(result, &balance); err != nil {
		return nil, err
	}

	balanceInt, ok := new(big.Int).SetString(strings.TrimPrefix(balance, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("failed to parse balance")
	}

	return balanceInt, nil
}

func (c *Client) GetCode(address string, blockNum *big.Int) (string, error) {
	var blockNumStr string
	if blockNum == nil {
		blockNumStr = "latest"
	} else {
		blockNumStr = fmt.Sprintf("0x%x", blockNum.Int64())
	}

	params := []string{address, blockNumStr}
	result, err := c.Call("eth_getCode", params)
	if err != nil {
		return nil, err
	}

	var code string
	if err := json.Unmarshal(result, &code); err != nil {
		return nil, err
	}

	return code, nil
}

func (c *Client) GetStorageAt(address string, slot string, blockNum *big.Int) (string, error) {
	var blockNumStr string
	if blockNum == nil {
		blockNumStr = "latest"
	} else {
		blockNumStr = fmt.Sprintf("0x%x", blockNum.Int64())
	}

	params := []string{address, slot, blockNumStr}
	result, err := c.Call("eth_getStorageAt", params)
	if err != nil {
		return nil, err
	}

	var storage string
	if err := json.Unmarshal(result, &storage); err != nil {
		return nil, err
	}

	return storage, nil
}

func (c *Client) GetTransactionCount(address string, blockNum *big.Int) (uint64, error) {
	var blockNumStr string
	if blockNum == nil {
		blockNumStr = "latest"
	} else {
		blockNumStr = fmt.Sprintf("0x%x", blockNum.Int64())
	}

	params := []string{address, blockNumStr}
	result, err := c.Call("eth_getTransactionCount", params)
	if err != nil {
		return 0, err
	}

	var nonceStr string
	if err := json.Unmarshal(result, &nonceStr); err != nil {
		return 0, err
	}

	nonce, ok := new(big.Int).SetString(strings.TrimPrefix(nonceStr, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("failed to parse nonce")
	}

	return nonce.Uint64(), nil
}

func (c *Client) Call(method string, params []interface{}) ([]byte, error) {
	request := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":     1,
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	startTime := time.Now()
	resp, err := c.httpClient.Do(req)
	c.mu.Lock()
	c.stats.Latency = time.Since(startTime)
	c.stats.Requests++
	if err != nil {
		c.stats.Errors++
		c.stats.LastError = err
	}
	c.mu.Unlock()

	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, ErrConnectionFailed
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rpcResponse map[string]interface{}
	if err := json.Unmarshal(responseBody, &rpcResponse); err != nil {
		return nil, err
	}

	if error, ok := rpcResponse["error"]; ok {
		errMsg := fmt.Sprintf("RPC error: %v", error)
		c.mu.Lock()
		c.stats.Errors++
		c.stats.LastError = errors.New(errMsg)
		c.mu.Unlock()
		return nil, errors.New(errMsg)
	}

	result, ok := rpcResponse["result"]
	if !ok {
		return nil, ErrInvalidResponse
	}

	return json.Marshal(result)
}

func (c *Client) Post(url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	return c.httpClient.Do(req)
}

func (c *Client) GetStats() RPCStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

func (c *Client) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

type Filter struct {
	FromBlock string   `json:"fromBlock,omitempty"`
	ToBlock   string   `json:"toBlock,omitempty"`
	Address  string   `json:"address,omitempty"`
	Topics   []string `json:"topics,omitempty"`
}

func (c *Client) GetLogs(filter Filter) ([]Log, error) {
	params := []interface{}{
		map[string]interface{}{
			"fromBlock": filter.FromBlock,
			"toBlock":   filter.ToBlock,
			"address":  filter.Address,
			"topics":   filter.Topics,
		},
	}

	result, err := c.Call("eth_getLogs", params)
	if err != nil {
		return nil, err
	}

	var logs []Log
	if err := json.Unmarshal(result, &logs); err != nil {
		return nil, err
	}

	return logs, nil
}

type ChainID int

func (c *Client) ChainID() (ChainID, error) {
	result, err := c.Call("eth_chainId", []interface{}{})
	if err != nil {
		return 0, err
	}

	var chainID string
	if err := json.Unmarshal(result, &chainID); err != nil {
		return 0, err
	}

	id, ok := new(big.Int).SetString(strings.TrimPrefix(chainID, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("failed to parse chain ID")
	}

	return ChainID(id.Int64()), nil
}

func (c *Client) BlockNumber() (uint64, error) {
	result, err := c.Call("eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, err
	}

	var blockNumStr string
	if err := json.Unmarshal(result, &blockNumStr); err != nil {
		return 0, err
	}

	blockNum, ok := new(big.Int).SetString(strings.TrimPrefix(blockNumStr, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("failed to parse block number")
	}

	return blockNum.Uint64(), nil
}

func (c *Client) GasPrice() (*big.Int, error) {
	result, err := c.Call("eth_gasPrice", []interface{}{})
	if err != nil {
		return nil, err
	}

	var gasPriceStr string
	if err := json.Unmarshal(result, &gasPriceStr); err != nil {
		return nil, err
	}

	gasPrice, ok := new(big.Int).SetString(strings.TrimPrefix(gasPriceStr, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("failed to parse gas price")
	}

	return gasPrice, nil
}

func (c *Client) EstimateGas(tx map[string]interface{}) (*big.Int, error) {
	params := []interface{}{tx}
	result, err := c.Call("eth_estimateGas", params)
	if err != nil {
		return nil, err
	}

	var gasStr string
	if err := json.Unmarshal(result, &gasStr); err != nil {
		return nil, err
	}

	gas, ok := new(big.Int).SetString(strings.TrimPrefix(gasStr, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("failed to parse gas estimate")
	}

	return gas, nil
}

func (c *Client) SendRawTransaction(signedTx string) (string, error) {
	params := []interface{}{signedTx}
	result, err := c.Call("eth_sendRawTransaction", params)
	if err != nil {
		return "", err
	}

	var txHash string
	if err := json.Unmarshal(result, &txHash); err != nil {
		return "", err
	}

	return txHash, nil
}

type BatchRequest struct {
	client *Client
	calls  [][]interface{}
	mu     sync.Mutex
	results []json.RawMessage
}

func (c *Client) NewBatch() *BatchRequest {
	return &BatchRequest{
		client: c,
		calls:  make([][]interface{}, 0, MaxBatchSize),
	}
}

func (b *BatchRequest) Add(method string, params []interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.calls) < MaxBatchSize {
		b.calls = append(b.calls, []interface{}{method, params})
	}
}

func (b *BatchRequest) Execute() ([]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.calls) == 0 {
		return nil, nil
	}

	requests := make([]map[string]interface{}, len(b.calls))
	for i, call := range b.calls {
		requests[i] = map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  call[0].(string),
			"params":  call[1].([]interface{}),
			"id":     i + 1,
		}
	}

	body, err := json.Marshal(requests)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", b.client.url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var responses []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responses); err != nil {
		return nil, err
	}

	results := make([]json.RawMessage, len(responses))
	for i, resp := range responses {
		if err, ok := resp["error"]; ok {
			return nil, fmt.Errorf("batch error: %v", err)
		}
		results[i], _ = json.Marshal(resp["result"])
	}

	b.results = results
	return results, nil
}

type Context struct {
	Client  *Client
	Cancel  context.CancelFunc
	Timeout time.Duration
}

func (c *Client) WithContext(ctx context.Context) *Context {
	ctx, cancel := context.WithCancel(ctx)
	return &Context{
		Client:  c,
		Cancel:  cancel,
		Timeout: DefaultTimeout,
	}
}

func (ctx *Context) Call(method string, params []interface{}) ([]byte, error) {
	type result struct {
		C <-chan error
	}

	return ctx.Client.Call(method, params)
}