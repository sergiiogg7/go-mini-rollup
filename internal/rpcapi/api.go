// Package rpcapi serves an Ethereum-compatible JSON-RPC API (enough for cast, forge, ethers and
// viem) plus the rollup_* namespace, on top of go-ethereum's rpc server.
package rpcapi

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/sergiiogg7/go-mini-rollup/internal/exec"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

// Chain is the read model the API needs. It has one implementation backed by the sequencer
// (unsafe head, accepts transactions) and one backed only by the derivation (verifier node).
type Chain interface {
	Resolve(tag rpc.BlockNumber) *proto.Block
	ByHash(h common.Hash) *proto.Block
	Tx(h common.Hash) (*gtypes.Transaction, *proto.Block, int, bool)
	PendingTx(h common.Hash) *gtypes.Transaction // nil when the hash is not waiting in the pool
	PendingNonce(a common.Address, stateNonce uint64) uint64
	Submit(raw []byte) (common.Hash, error)
	Status() map[string]any
}

var gwei = big.NewInt(1_000_000_000)

type API struct {
	chain   Chain
	ex      *exec.Executor
	chainID uint64
	version string
	signer  gtypes.Signer
}

// New builds the JSON-RPC handler.
func New(chain Chain, ex *exec.Executor, chainID uint64, version string) (http.Handler, error) {
	a := &API{chain: chain, ex: ex, chainID: chainID, version: version, signer: gtypes.LatestSignerForChainID(new(big.Int).SetUint64(chainID))}
	srv := rpc.NewServer()
	for ns, svc := range map[string]any{"eth": &EthAPI{a}, "net": &NetAPI{a}, "web3": &Web3API{a}, "rollup": &RollupAPI{a}} {
		if err := srv.RegisterName(ns, svc); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

type EthAPI struct{ *API }
type NetAPI struct{ *API }
type Web3API struct{ *API }
type RollupAPI struct{ *API }

func (a *NetAPI) Version() string               { return fmt.Sprint(a.chainID) }
func (a *NetAPI) Listening() bool               { return true }
func (a *NetAPI) PeerCount() hexutil.Uint       { return 0 }
func (a *Web3API) ClientVersion() string        { return a.version }
func (a *RollupAPI) SyncStatus() map[string]any { return a.chain.Status() }

func (a *EthAPI) ChainId() *hexutil.Big              { return (*hexutil.Big)(new(big.Int).SetUint64(a.chainID)) }
func (a *EthAPI) Syncing() bool                      { return false }
func (a *EthAPI) Accounts() []common.Address         { return []common.Address{} }
func (a *EthAPI) Mining() bool                       { return false }
func (a *EthAPI) GasPrice() *hexutil.Big             { return (*hexutil.Big)(gwei) }
func (a *EthAPI) MaxPriorityFeePerGas() *hexutil.Big { return (*hexutil.Big)(gwei) }
func (a *EthAPI) BlobBaseFee() *hexutil.Big          { return (*hexutil.Big)(big.NewInt(1)) }

func (a *EthAPI) BlockNumber() hexutil.Uint64 {
	return hexutil.Uint64(a.chain.Resolve(rpc.LatestBlockNumber).Number())
}

func (a *EthAPI) FeeHistory(count hexutil.Uint64, newest rpc.BlockNumber, _ []float64) map[string]any {
	n := uint64(0)
	if b := a.chain.Resolve(newest); b != nil {
		n = b.Number()
	}
	if uint64(count) > n+1 {
		count = hexutil.Uint64(n + 1)
	}
	base := make([]*hexutil.Big, count+1)
	ratio := make([]float64, count)
	reward := make([][]*hexutil.Big, count)
	for i := range base {
		base[i] = (*hexutil.Big)(big.NewInt(0))
	}
	for i := range reward {
		reward[i] = []*hexutil.Big{(*hexutil.Big)(gwei)}
	}
	return map[string]any{"oldestBlock": hexutil.Uint64(n + 1 - uint64(count)), "baseFeePerGas": base, "gasUsedRatio": ratio, "reward": reward}
}

// block resolves a block selector (number, tag or hash).
func (a *API) block(bnh *rpc.BlockNumberOrHash) (*proto.Block, error) {
	if bnh == nil {
		return a.chain.Resolve(rpc.LatestBlockNumber), nil
	}
	if h, ok := bnh.Hash(); ok {
		if b := a.chain.ByHash(h); b != nil {
			return b, nil
		}
		return nil, errors.New("header for hash not found")
	}
	n, _ := bnh.Number()
	if b := a.chain.Resolve(n); b != nil {
		return b, nil
	}
	return nil, errors.New("block not found")
}

func (a *API) stateAt(bnh *rpc.BlockNumberOrHash) (*proto.Block, error) { return a.block(bnh) }

func (a *EthAPI) GetBalance(addr common.Address, bnh *rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	b, err := a.block(bnh)
	if err != nil {
		return nil, err
	}
	st, err := a.ex.StateAt(b.Header.Root)
	if err != nil {
		return nil, err
	}
	return (*hexutil.Big)(st.GetBalance(addr).ToBig()), nil
}

func (a *EthAPI) GetTransactionCount(addr common.Address, bnh *rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	b, err := a.block(bnh)
	if err != nil {
		return 0, err
	}
	st, err := a.ex.StateAt(b.Header.Root)
	if err != nil {
		return 0, err
	}
	n := st.GetNonce(addr)
	if bnh != nil {
		if tag, ok := bnh.Number(); ok && tag == rpc.PendingBlockNumber {
			n = a.chain.PendingNonce(addr, n)
		}
	}
	return hexutil.Uint64(n), nil
}

func (a *EthAPI) GetCode(addr common.Address, bnh *rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	b, err := a.block(bnh)
	if err != nil {
		return nil, err
	}
	st, err := a.ex.StateAt(b.Header.Root)
	if err != nil {
		return nil, err
	}
	return st.GetCode(addr), nil
}

func (a *EthAPI) GetStorageAt(addr common.Address, key common.Hash, bnh *rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	b, err := a.block(bnh)
	if err != nil {
		return nil, err
	}
	st, err := a.ex.StateAt(b.Header.Root)
	if err != nil {
		return nil, err
	}
	v := st.GetState(addr, key)
	return v[:], nil
}

// CallArgs are the eth_call / eth_estimateGas arguments.
type CallArgs struct {
	From  *common.Address `json:"from"`
	To    *common.Address `json:"to"`
	Gas   *hexutil.Uint64 `json:"gas"`
	Value *hexutil.Big    `json:"value"`
	Data  *hexutil.Bytes  `json:"data"`
	Input *hexutil.Bytes  `json:"input"`
}

func (c CallArgs) msg() exec.CallMsg {
	m := exec.CallMsg{To: c.To}
	if c.From != nil {
		m.From = *c.From
	}
	if c.Gas != nil {
		m.Gas = uint64(*c.Gas)
	}
	if c.Value != nil {
		m.Value = c.Value.ToInt()
	}
	switch {
	case c.Input != nil:
		m.Data = *c.Input
	case c.Data != nil:
		m.Data = *c.Data
	}
	return m
}

type revertError struct {
	msg  string
	data hexutil.Bytes
}

func (e *revertError) Error() string          { return e.msg }
func (e *revertError) ErrorCode() int         { return 3 }
func (e *revertError) ErrorData() interface{} { return e.data }

func (a *EthAPI) run(args CallArgs, bnh *rpc.BlockNumberOrHash) (uint64, []byte, error) {
	b, err := a.block(bnh)
	if err != nil {
		return 0, nil, err
	}
	res, err := a.ex.Call(b.Header.Root, b.Number()+1, b.Time()+1, args.msg())
	if err != nil {
		return 0, nil, err
	}
	if res.Err != nil {
		return res.UsedGas, res.ReturnData, &revertError{msg: "execution reverted: " + res.Err.Error(), data: res.ReturnData}
	}
	return res.UsedGas, res.ReturnData, nil
}

func (a *EthAPI) Call(args CallArgs, bnh *rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	_, out, err := a.run(args, bnh)
	return out, err
}

func (a *EthAPI) EstimateGas(args CallArgs, bnh *rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	used, _, err := a.run(args, bnh)
	if err != nil {
		return 0, err
	}
	if used <= 21000 {
		return 21000, nil
	}
	return hexutil.Uint64(used*13/10 + 5000), nil // headroom for refunds and the 63/64 rule
}

func (a *EthAPI) SendRawTransaction(raw hexutil.Bytes) (common.Hash, error) {
	return a.chain.Submit(raw)
}

func (a *EthAPI) GetBlockByNumber(n rpc.BlockNumber, full bool) map[string]any {
	if b := a.chain.Resolve(n); b != nil {
		return a.blockJSON(b, full)
	}
	return nil
}

func (a *EthAPI) GetBlockByHash(h common.Hash, full bool) map[string]any {
	if b := a.chain.ByHash(h); b != nil {
		return a.blockJSON(b, full)
	}
	return nil
}

func (a *EthAPI) GetTransactionByHash(h common.Hash) map[string]any {
	if tx, b, i, ok := a.chain.Tx(h); ok {
		return a.txJSON(tx, b, i)
	}
	if tx := a.chain.PendingTx(h); tx != nil {
		return a.txJSON(tx, nil, 0) // blockHash, blockNumber and transactionIndex are null
	}
	return nil
}

func (a *EthAPI) GetTransactionReceipt(h common.Hash) map[string]any {
	tx, b, i, ok := a.chain.Tx(h)
	if !ok {
		return nil
	}
	rc := b.Receipts[i]
	from, _ := gtypes.Sender(a.signer, tx)
	logs := make([]map[string]any, len(rc.Logs))
	for j, l := range rc.Logs {
		logs[j] = map[string]any{
			"address": l.Address, "topics": l.Topics, "data": hexutil.Bytes(l.Data), "blockNumber": hexutil.Uint64(b.Number()),
			"transactionHash": h, "transactionIndex": hexutil.Uint64(i), "blockHash": b.Hash(), "logIndex": hexutil.Uint64(l.Index), "removed": false,
		}
	}
	var contract any
	if rc.ContractAddress != (common.Address{}) {
		contract = rc.ContractAddress
	}
	return map[string]any{
		"transactionHash": h, "transactionIndex": hexutil.Uint64(i), "blockHash": b.Hash(), "blockNumber": hexutil.Uint64(b.Number()),
		"from": from, "to": tx.To(), "cumulativeGasUsed": hexutil.Uint64(rc.CumulativeGasUsed), "gasUsed": hexutil.Uint64(rc.GasUsed),
		"contractAddress": contract, "logs": logs, "logsBloom": hexutil.Bytes(rc.Bloom[:]), "status": hexutil.Uint64(rc.Status),
		"type": hexutil.Uint64(tx.Type()), "effectiveGasPrice": (*hexutil.Big)(tx.GasPrice()),
	}
}

func (a *EthAPI) GetLogs(crit filters.FilterCriteria) ([]map[string]any, error) {
	q := ethereum.FilterQuery(crit)
	var blocks []*proto.Block
	if q.BlockHash != nil {
		if b := a.chain.ByHash(*q.BlockHash); b != nil {
			blocks = append(blocks, b)
		}
	} else {
		head := a.chain.Resolve(rpc.LatestBlockNumber).Number()
		from, to := uint64(0), head
		if q.FromBlock != nil && q.FromBlock.Sign() >= 0 {
			from = q.FromBlock.Uint64()
		}
		if q.ToBlock != nil && q.ToBlock.Sign() >= 0 {
			to = min(q.ToBlock.Uint64(), head)
		}
		if to-from > 100_000 {
			return nil, errors.New("block range too large")
		}
		for n := from; n <= to; n++ {
			if b := a.chain.Resolve(rpc.BlockNumber(n)); b != nil {
				blocks = append(blocks, b)
			}
		}
	}
	out := []map[string]any{}
	for _, b := range blocks {
		for i, rc := range b.Receipts {
			for _, l := range rc.Logs {
				if !match(l, q) {
					continue
				}
				out = append(out, map[string]any{
					"address": l.Address, "topics": l.Topics, "data": hexutil.Bytes(l.Data), "blockNumber": hexutil.Uint64(b.Number()),
					"transactionHash": rc.TxHash, "transactionIndex": hexutil.Uint64(i), "blockHash": b.Hash(), "logIndex": hexutil.Uint64(l.Index), "removed": false,
				})
			}
		}
	}
	return out, nil
}

func match(l *gtypes.Log, q ethereum.FilterQuery) bool {
	if len(q.Addresses) > 0 {
		ok := false
		for _, a := range q.Addresses {
			ok = ok || a == l.Address
		}
		if !ok {
			return false
		}
	}
	for i, alts := range q.Topics {
		if len(alts) == 0 {
			continue
		}
		if i >= len(l.Topics) {
			return false
		}
		ok := false
		for _, t := range alts {
			ok = ok || t == l.Topics[i]
		}
		if !ok {
			return false
		}
	}
	return true
}

func (a *API) blockJSON(b *proto.Block, full bool) map[string]any {
	h := b.Header
	txs := make([]any, len(b.Txs))
	for i, tx := range b.Txs {
		if full {
			txs[i] = a.txJSON(tx, b, i)
		} else {
			txs[i] = tx.Hash()
		}
	}
	return map[string]any{
		"number": hexutil.Uint64(b.Number()), "hash": b.Hash(), "parentHash": h.ParentHash, "nonce": "0x0000000000000000",
		"sha3Uncles": h.UncleHash, "logsBloom": hexutil.Bytes(h.Bloom[:]), "transactionsRoot": h.TxHash, "stateRoot": h.Root,
		"receiptsRoot": h.ReceiptHash, "miner": h.Coinbase, "difficulty": "0x0", "totalDifficulty": "0x0",
		"extraData": hexutil.Bytes(h.Extra), "size": hexutil.Uint64(512), "gasLimit": hexutil.Uint64(h.GasLimit),
		"gasUsed": hexutil.Uint64(h.GasUsed), "timestamp": hexutil.Uint64(h.Time), "baseFeePerGas": (*hexutil.Big)(h.BaseFee),
		"mixHash": common.Hash{}, "transactions": txs, "uncles": []common.Hash{},
		"l1OriginNumber": hexutil.Uint64(b.OriginNumber), "l1OriginHash": b.OriginHash,
	}
}

func (a *API) txJSON(tx *gtypes.Transaction, b *proto.Block, i int) map[string]any {
	v, r, s := tx.RawSignatureValues()
	from, _ := gtypes.Sender(a.signer, tx)
	m := map[string]any{
		"hash": tx.Hash(), "nonce": hexutil.Uint64(tx.Nonce()), "from": from, "to": tx.To(), "value": (*hexutil.Big)(tx.Value()),
		"gas": hexutil.Uint64(tx.Gas()), "gasPrice": (*hexutil.Big)(tx.GasPrice()), "input": hexutil.Bytes(tx.Data()),
		"type": hexutil.Uint64(tx.Type()), "chainId": (*hexutil.Big)(tx.ChainId()), "v": (*hexutil.Big)(v), "r": (*hexutil.Big)(r), "s": (*hexutil.Big)(s),
	}
	if b != nil {
		m["blockHash"], m["blockNumber"], m["transactionIndex"] = b.Hash(), hexutil.Uint64(b.Number()), hexutil.Uint64(i)
	} else {
		m["blockHash"], m["blockNumber"], m["transactionIndex"] = nil, nil, nil
	}
	if tx.Type() >= gtypes.DynamicFeeTxType {
		m["maxFeePerGas"], m["maxPriorityFeePerGas"] = (*hexutil.Big)(tx.GasFeeCap()), (*hexutil.Big)(tx.GasTipCap())
		m["accessList"], m["yParity"] = tx.AccessList(), (*hexutil.Big)(v)
	}
	return m
}
