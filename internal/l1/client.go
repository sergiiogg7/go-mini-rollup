// Package l1 adapts a real Ethereum JSON-RPC node (Anvil, Geth, ...) to the ports used by the
// derivation (read side) and the batcher (write side).
package l1

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

// depositTopic is keccak256("Deposit(address,address,uint256,uint256)").
var depositTopic = crypto.Keccak256Hash([]byte("Deposit(address,address,uint256,uint256)"))

// Client talks to L1.
type Client struct {
	c      *ethclient.Client
	inbox  common.Address
	portal common.Address
	mu     sync.Mutex
	chain  *big.Int
}

const timeout = 10 * time.Second

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// Dial connects to an L1 RPC endpoint.
func Dial(url string, inbox, portal common.Address) (*Client, error) {
	c, err := ethclient.Dial(url)
	if err != nil {
		return nil, err
	}
	return &Client{c: c, inbox: inbox, portal: portal}, nil
}

func (l *Client) Close() { l.c.Close() }

func (l *Client) chainID() (*big.Int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.chain != nil {
		return l.chain, nil
	}
	cx, cancel := ctx()
	defer cancel()
	id, err := l.c.ChainID(cx)
	if err != nil {
		return nil, err
	}
	l.chain = id
	return id, nil
}

func (l *Client) ChainID() (uint64, error) {
	id, err := l.chainID()
	if err != nil {
		return 0, err
	}
	return id.Uint64(), nil
}

// Head returns the latest L1 block number.
func (l *Client) Head() (uint64, error) {
	cx, cancel := ctx()
	defer cancel()
	return l.c.BlockNumber(cx)
}

// Block returns the data the derivation needs from L1 block n, or nil if it does not exist.
// Logs are queried by block hash, never by number range, so a reorg cannot mix two blocks.
func (l *Client) Block(n uint64) (*proto.L1BlockData, error) {
	cx, cancel := ctx()
	defer cancel()
	blk, err := l.c.BlockByNumber(cx, new(big.Int).SetUint64(n))
	if err != nil {
		if errors.Is(err, ethereum.NotFound) {
			return nil, nil
		}
		return nil, err
	}
	id, err := l.chainID()
	if err != nil {
		return nil, err
	}
	signer := gtypes.LatestSignerForChainID(id)
	data := &proto.L1BlockData{Ref: proto.L1BlockRef{
		Number: blk.NumberU64(), Hash: blk.Hash(), ParentHash: blk.ParentHash(), Time: blk.Time(),
	}}
	for _, tx := range blk.Transactions() {
		if tx.To() == nil || *tx.To() != l.inbox {
			continue
		}
		from, err := gtypes.Sender(signer, tx)
		if err != nil {
			continue // an inbox transaction with an invalid signature cannot be authentic
		}
		data.Batches = append(data.Batches, proto.BatchTx{TxHash: tx.Hash(), From: from, Data: tx.Data()})
	}
	if l.portal != (common.Address{}) {
		h := blk.Hash()
		logs, err := l.c.FilterLogs(cx, ethereum.FilterQuery{BlockHash: &h, Addresses: []common.Address{l.portal}, Topics: [][]common.Hash{{depositTopic}}})
		if err != nil {
			return nil, err
		}
		for _, lg := range logs {
			if len(lg.Topics) != 3 || len(lg.Data) != 64 {
				continue
			}
			data.Deposits = append(data.Deposits, proto.Deposit{
				ID:    new(big.Int).SetBytes(lg.Data[32:]).Uint64(),
				To:    common.BytesToAddress(lg.Topics[2].Bytes()),
				Value: new(big.Int).SetBytes(lg.Data[:32]),
			})
		}
	}
	return data, nil
}

// ---------------------------------------------------------------- write side (batcher)

func (l *Client) Nonce(a common.Address) (uint64, error) {
	cx, cancel := ctx()
	defer cancel()
	return l.c.NonceAt(cx, a, nil)
}

func (l *Client) PendingNonce(a common.Address) (uint64, error) {
	cx, cancel := ctx()
	defer cancel()
	return l.c.PendingNonceAt(cx, a)
}

func (l *Client) GasPrice() (*big.Int, error) {
	cx, cancel := ctx()
	defer cancel()
	return l.c.SuggestGasPrice(cx)
}

func (l *Client) Send(raw []byte) (common.Hash, error) {
	tx := new(gtypes.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	cx, cancel := ctx()
	defer cancel()
	if err := l.c.SendTransaction(cx, tx); err != nil {
		return common.Hash{}, err
	}
	return tx.Hash(), nil
}

// Receipt returns nil, nil while the transaction is not in the canonical chain.
func (l *Client) Receipt(h common.Hash) (*proto.TxInclusion, error) {
	cx, cancel := ctx()
	defer cancel()
	r, err := l.c.TransactionReceipt(cx, h)
	if err != nil {
		if errors.Is(err, ethereum.NotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &proto.TxInclusion{BlockNumber: r.BlockNumber.Uint64(), BlockHash: r.BlockHash}, nil
}
