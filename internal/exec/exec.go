// Package exec executes L2 blocks on top of go-ethereum's real EVM and state (Merkle Patricia
// Trie, StateDB). It is deterministic: the sequencer and the derivation both call Execute with
// the same inputs and must produce the same block hash.
package exec

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"

	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

// Executor owns the in-memory state database. States are addressed by root, so keeping a root is
// a free snapshot: rolling back after an L1 reorg only means going back to an older root.
type Executor struct {
	cfg      *params.ChainConfig
	db       state.Database
	gasLimit uint64
	coinbase common.Address

	mu     sync.RWMutex
	hashes map[uint64]common.Hash // L2 block hashes for the BLOCKHASH opcode
}

// New builds an executor for the given chain id.
func New(chainID, gasLimit uint64, coinbase common.Address) *Executor {
	cfg := *params.MergedTestChainConfig
	cfg.ChainID = new(big.Int).SetUint64(chainID)
	mdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(mdb, nil)
	return &Executor{
		cfg: &cfg, db: state.NewDatabase(tdb, state.NewCodeDB(mdb)),
		gasLimit: gasLimit, coinbase: coinbase, hashes: map[uint64]common.Hash{},
	}
}

// ChainConfig exposes the EVM chain configuration (needed to build signers).
func (e *Executor) ChainConfig() *params.ChainConfig { return e.cfg }

// SetBlockHash records a canonical block hash for BLOCKHASH; passing the zero hash forgets it.
func (e *Executor) SetBlockHash(n uint64, h common.Hash) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h == (common.Hash{}) {
		delete(e.hashes, n)
		return
	}
	e.hashes[n] = h
}

func (e *Executor) getHash(n uint64) common.Hash {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.hashes[n]
}

// Genesis commits the genesis allocation and returns the L2 genesis block.
func (e *Executor) Genesis(c *proto.Config) (*proto.Block, error) {
	st, err := state.New(gtypes.EmptyRootHash, e.db)
	if err != nil {
		return nil, err
	}
	for a, v := range c.Alloc {
		st.AddBalance(a, uint256.MustFromBig(v), tracing.BalanceChangeUnspecified)
	}
	root, err := st.Commit(e.cfg.Rules(big.NewInt(0), true, c.GenesisTime), 0)
	if err != nil {
		return nil, err
	}
	var origin common.Hash
	h := &gtypes.Header{
		Root: root, Number: big.NewInt(0), GasLimit: e.gasLimit, Time: c.GenesisTime,
		Difficulty: big.NewInt(0), BaseFee: big.NewInt(0),
		TxHash: gtypes.EmptyTxsHash, ReceiptHash: gtypes.EmptyReceiptsHash, UncleHash: gtypes.EmptyUncleHash,
		Coinbase: e.coinbase, Extra: extra(c.L1Start-1, origin, nil),
	}
	return proto.NewBlock(h, nil, nil, nil, c.L1Start-1, origin), nil
}

func extra(originNumber uint64, originHash common.Hash, deps []proto.Deposit) []byte {
	var dh common.Hash
	if len(deps) > 0 {
		enc, _ := rlp.EncodeToBytes(deps)
		dh = crypto.Keccak256Hash(enc)
	}
	b, _ := rlp.EncodeToBytes([]any{originNumber, originHash, dh})
	return b
}

// Input is everything needed to build one L2 block.
type Input struct {
	Number       uint64
	Time         uint64
	ParentHash   common.Hash
	OriginNumber uint64
	OriginHash   common.Hash
	Deposits     []proto.Deposit
	Txs          []*gtypes.Transaction
}

// Skip records a transaction that was not included and why.
type Skip struct {
	Hash   common.Hash
	Reason string
}

// Result is an executed block plus the skipped transactions.
type Result struct {
	Block   *proto.Block
	Skipped []Skip
}

func (e *Executor) blockContext(number, time uint64) vm.BlockContext {
	rnd := common.Hash{}
	return vm.BlockContext{
		CanTransfer: core.CanTransfer, Transfer: core.Transfer, GetHash: e.getHash,
		Coinbase: e.coinbase, GasLimit: e.gasLimit, BlockNumber: new(big.Int).SetUint64(number), Time: time,
		Difficulty: big.NewInt(0), BaseFee: big.NewInt(0), BlobBaseFee: big.NewInt(1), Random: &rnd,
	}
}

// Execute builds block `in` on top of the state `parentRoot`. Deposits are applied first, then the
// transactions in order. A transaction the protocol deems invalid (bad signature, nonce, funds, gas
// limit, blob type, unprotected) is skipped deterministically and never aborts the block.
func (e *Executor) Execute(parentRoot common.Hash, in Input) (*Result, error) {
	st, err := state.New(parentRoot, e.db)
	if err != nil {
		return nil, fmt.Errorf("open parent state: %w", err)
	}
	num := new(big.Int).SetUint64(in.Number)
	rules := e.cfg.Rules(num, true, in.Time)
	for _, d := range in.Deposits {
		st.AddBalance(d.To, uint256.MustFromBig(d.Value), tracing.BalanceChangeUnspecified)
	}
	bctx := e.blockContext(in.Number, in.Time)
	evm := vm.NewEVM(bctx, st, e.cfg, vm.Config{})
	defer evm.Release()
	signer := gtypes.MakeSigner(e.cfg, num, in.Time)
	gp := core.NewGasPool(e.gasLimit)

	var (
		included []*gtypes.Transaction
		receipts gtypes.Receipts
		skipped  []Skip
	)
	for _, tx := range in.Txs {
		if reason := precheck(tx); reason != "" {
			skipped = append(skipped, Skip{tx.Hash(), reason})
			continue
		}
		msg, err := core.TransactionToMessage(tx, signer, bctx.BaseFee)
		if err != nil {
			skipped = append(skipped, Skip{tx.Hash(), "signature: " + err.Error()})
			continue
		}
		snap := st.Snapshot()
		st.SetTxContext(tx.Hash(), len(included), uint32(len(included)+1))
		rc, _, err := core.ApplyTransactionWithEVM(context.Background(), msg, gp, st, num, common.Hash{}, in.Time, tx, evm)
		if err != nil {
			st.RevertToSnapshot(snap)
			skipped = append(skipped, Skip{tx.Hash(), "invalid: " + err.Error()})
			continue
		}
		included = append(included, tx)
		receipts = append(receipts, rc)
	}
	root, err := st.Commit(rules, in.Number)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	var gasUsed uint64
	if n := len(receipts); n > 0 {
		gasUsed = receipts[n-1].CumulativeGasUsed
	}
	h := &gtypes.Header{
		ParentHash: in.ParentHash, Coinbase: e.coinbase, Root: root,
		TxHash:      gtypes.DeriveSha(gtypes.Transactions(included), trie.NewStackTrie(nil)),
		ReceiptHash: gtypes.DeriveSha(receipts, trie.NewStackTrie(nil)),
		Bloom:       gtypes.MergeBloom(receipts), Difficulty: big.NewInt(0), Number: num,
		GasLimit: e.gasLimit, GasUsed: gasUsed, Time: in.Time, BaseFee: big.NewInt(0),
		UncleHash: gtypes.EmptyUncleHash, Extra: extra(in.OriginNumber, in.OriginHash, in.Deposits),
	}
	blk := proto.NewBlock(h, included, receipts, in.Deposits, in.OriginNumber, in.OriginHash)
	for i, rc := range receipts {
		rc.BlockHash = blk.Hash()
		for _, l := range rc.Logs {
			l.BlockHash = blk.Hash()
		}
		rc.TransactionIndex = uint(i)
	}
	return &Result{Block: blk, Skipped: skipped}, nil
}

func precheck(tx *gtypes.Transaction) string {
	switch {
	case tx.Type() == gtypes.BlobTxType:
		return "blob transactions are not supported"
	case !tx.Protected():
		return "unprotected transaction (no chain id)"
	}
	return ""
}

// MaxCallGas is the gas cap for read-only calls (below the per-transaction cap of recent forks).
const MaxCallGas = 16_000_000

// CallMsg is a read-only call (eth_call, eth_estimateGas).
type CallMsg struct {
	From  common.Address
	To    *common.Address
	Gas   uint64
	Value *big.Int
	Data  []byte
}

// Call runs msg on the state `root` as if it were mined in block (number, time). Nothing is persisted.
func (e *Executor) Call(root common.Hash, number, time uint64, m CallMsg) (*core.ExecutionResult, error) {
	st, err := state.New(root, e.db)
	if err != nil {
		return nil, err
	}
	gas := m.Gas
	if gas == 0 || gas > MaxCallGas {
		gas = MaxCallGas
	}
	val := new(uint256.Int)
	if m.Value != nil {
		val = uint256.MustFromBig(m.Value)
	}
	evm := vm.NewEVM(e.blockContext(number, time), st, e.cfg, vm.Config{NoBaseFee: true})
	defer evm.Release()
	msg := &core.Message{
		From: m.From, To: m.To, Nonce: st.GetNonce(m.From), Value: val, GasLimit: gas,
		GasPrice: new(uint256.Int), GasFeeCap: new(uint256.Int), GasTipCap: new(uint256.Int), Data: m.Data,
	}
	return core.ApplyMessage(evm, msg, core.NewGasPool(e.gasLimit))
}

// StateAt opens a read-only view of a state root.
func (e *Executor) StateAt(root common.Hash) (*state.StateDB, error) {
	st, err := state.New(root, e.db)
	if err != nil {
		return nil, errors.New("state not available: " + err.Error())
	}
	return st, nil
}
