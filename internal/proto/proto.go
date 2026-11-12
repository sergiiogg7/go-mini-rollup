// Package proto holds the protocol types shared by every component: L1 references,
// deposits, blocks, parameters, the fork schedule and the epoch batch codec.
// It depends only on go-ethereum value types (no I/O).
package proto

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// L1BlockRef identifies an L1 block.
type L1BlockRef struct {
	Number     uint64
	Hash       common.Hash
	ParentHash common.Hash
	Time       uint64
}

// Deposit is an L1 deposit that mints value on L2.
type Deposit struct {
	ID    uint64
	To    common.Address
	Value *big.Int
}

// BatchTx is an L1 transaction sent to the batch inbox.
type BatchTx struct {
	TxHash common.Hash
	From   common.Address
	Data   []byte
}

// L1BlockData is everything the derivation needs from one L1 block.
type L1BlockData struct {
	Ref      L1BlockRef
	Deposits []Deposit
	Batches  []BatchTx
}

// Draft is the content of a closed epoch as produced by the sequencer, ready to be batched.
type Draft struct {
	Epoch      uint64
	OriginHash common.Hash
	Blocks     []BatchBlock
}

// TxInclusion says where an L1 transaction was mined.
type TxInclusion struct {
	BlockNumber uint64
	BlockHash   common.Hash
}

// Params are the protocol parameters in force for an epoch (selected by the fork schedule).
type Params struct {
	ChainID              uint64
	MaxBlocksPerEpoch    int
	SeqWindow            uint64
	MaxDriftSeconds      uint64
	OriginConfirmations  uint64
	FinalityDepth        uint64
	MaxCompressedBytes   int
	MaxDecompressedBytes int
	MaxTxBytes           int
	BatchVersion         uint8
	GasLimit             uint64
}

// Fork activates Params from the epoch (L1 block number) Activation onwards.
type Fork struct {
	Activation uint64
	Params     Params
}

// Config is the rollup configuration. It is the genesis of the L2.
type Config struct {
	L1Start     uint64 // first epoch
	GenesisTime uint64
	Inbox       common.Address // batch inbox (an address with no code)
	Batcher     common.Address // only sender whose inbox transactions count
	Portal      common.Address // L1 deposit contract
	FeeVault    common.Address // L2 coinbase
	Alloc       map[common.Address]*big.Int
	Forks       []Fork // sorted by Activation, first one at or before L1Start
}

// V1 returns the genesis parameters.
func V1(chainID uint64) Params {
	return Params{
		ChainID: chainID, MaxBlocksPerEpoch: 16, SeqWindow: 8, MaxDriftSeconds: 600,
		OriginConfirmations: 1, FinalityDepth: 12, MaxCompressedBytes: 120_000,
		MaxDecompressedBytes: 1 << 20, MaxTxBytes: 128 * 1024, BatchVersion: 0, GasLimit: 30_000_000,
	}
}

// V2 is the first fork: bigger epochs, longer drift and batches that commit to the origin hash.
func V2(chainID uint64) Params {
	p := V1(chainID)
	p.MaxBlocksPerEpoch = 32
	p.MaxDriftSeconds = 1800
	p.BatchVersion = 1
	return p
}

// ParamsAt returns the parameters for an epoch.
func (c *Config) ParamsAt(epoch uint64) Params {
	p := c.Forks[0].Params
	for _, f := range c.Forks {
		if f.Activation <= epoch {
			p = f.Params
		}
	}
	return p
}

// Validate checks the configuration is internally consistent.
func (c *Config) Validate() error {
	if len(c.Forks) == 0 {
		return errors.New("no forks configured")
	}
	for i, f := range c.Forks {
		if i > 0 && f.Activation <= c.Forks[i-1].Activation {
			return errors.New("forks must be strictly increasing")
		}
	}
	if c.Forks[0].Activation > c.L1Start {
		return errors.New("first fork must be active at L1Start")
	}
	return nil
}

// Block is an L2 block with its execution results.
type Block struct {
	Header       *gtypes.Header
	Txs          []*gtypes.Transaction
	Receipts     gtypes.Receipts
	Deposits     []Deposit
	OriginNumber uint64
	OriginHash   common.Hash
	hash         common.Hash
}

// NewBlock wraps an executed block and caches its hash.
func NewBlock(h *gtypes.Header, txs []*gtypes.Transaction, rc gtypes.Receipts, deps []Deposit, on uint64, oh common.Hash) *Block {
	return &Block{Header: h, Txs: txs, Receipts: rc, Deposits: deps, OriginNumber: on, OriginHash: oh, hash: h.Hash()}
}

func (b *Block) Hash() common.Hash { return b.hash }
func (b *Block) Number() uint64    { return b.Header.Number.Uint64() }
func (b *Block) Time() uint64      { return b.Header.Time }

// ---------------------------------------------------------------- batch codec

// BatchBlock is one L2 block inside a batch: only a timestamp and raw transactions.
// The batch carries no hashes; the derivation recomputes everything.
type BatchBlock struct {
	Time uint64
	Txs  [][]byte
}

// EpochBatch is everything the sequencer produced for one epoch.
type EpochBatch struct {
	Version    uint8
	Epoch      uint64
	OriginHash common.Hash // version 1 only
	Blocks     []BatchBlock
}

type batchV0 struct {
	Epoch  uint64
	Blocks []BatchBlock
}

type batchV1 struct {
	Epoch  uint64
	Origin common.Hash
	Blocks []BatchBlock
}

// ErrDecode is returned (wrapped) for every malformed batch.
var ErrDecode = errors.New("batch decode error")

// EncodeBatch produces version byte || zlib(rlp(batch)).
func EncodeBatch(b *EpochBatch) ([]byte, error) {
	var payload []byte
	var err error
	switch b.Version {
	case 0:
		payload, err = rlp.EncodeToBytes(&batchV0{b.Epoch, b.Blocks})
	case 1:
		payload, err = rlp.EncodeToBytes(&batchV1{b.Epoch, b.OriginHash, b.Blocks})
	default:
		return nil, fmt.Errorf("unknown batch version %d", b.Version)
	}
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteByte(b.Version)
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeBatch parses a batch. It never panics and never allocates more than maxDecompressed.
func DecodeBatch(data []byte, maxCompressed, maxDecompressed int) (*EpochBatch, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("%w: too short", ErrDecode)
	}
	if len(data) > maxCompressed {
		return nil, fmt.Errorf("%w: compressed size %d over limit", ErrDecode, len(data))
	}
	version := data[0]
	zr, err := zlib.NewReader(bytes.NewReader(data[1:]))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, int64(maxDecompressed)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: inflate: %v", ErrDecode, err)
	}
	if len(raw) > maxDecompressed {
		return nil, fmt.Errorf("%w: decompressed size over limit", ErrDecode)
	}
	switch version {
	case 0:
		var v batchV0
		if err := rlp.DecodeBytes(raw, &v); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDecode, err)
		}
		return &EpochBatch{Version: 0, Epoch: v.Epoch, Blocks: v.Blocks}, nil
	case 1:
		var v batchV1
		if err := rlp.DecodeBytes(raw, &v); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDecode, err)
		}
		return &EpochBatch{Version: 1, Epoch: v.Epoch, OriginHash: v.Origin, Blocks: v.Blocks}, nil
	default:
		return nil, fmt.Errorf("%w: unknown version %d", ErrDecode, version)
	}
}
