package sequencer

import (
	"errors"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrKnown             = errors.New("already known")
	ErrNonceTooLow       = errors.New("nonce too low")
	ErrNonceGap          = errors.New("nonce too far in the future")
	ErrInsufficientFunds = errors.New("insufficient funds for value + gas")
	ErrPoolFull          = errors.New("txpool is full")
	ErrUnderpriced       = errors.New("replacement transaction underpriced")
)

const maxNonceGap = 16

type entry struct {
	tx   *gtypes.Transaction
	from common.Address
	seq  uint64
}

// Pool holds pending transactions, per sender ordered by nonce, globally by arrival.
type Pool struct {
	mu       sync.Mutex
	byHash   map[common.Hash]*entry
	bySender map[common.Address][]*entry
	seq      uint64
	max      int
}

func NewPool(max int) *Pool {
	return &Pool{byHash: map[common.Hash]*entry{}, bySender: map[common.Address][]*entry{}, max: max}
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byHash)
}

func (p *Pool) Has(h common.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byHash[h] != nil
}

// Add validates against the sender's current state nonce and balance and inserts the transaction.
func (p *Pool) Add(tx *gtypes.Transaction, from common.Address, stateNonce uint64, balance *big.Int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byHash[tx.Hash()] != nil {
		return ErrKnown
	}
	if tx.Nonce() < stateNonce {
		return ErrNonceTooLow
	}
	if tx.Nonce() > stateNonce+maxNonceGap {
		return ErrNonceGap
	}
	if balance.Cmp(tx.Cost()) < 0 {
		return ErrInsufficientFunds
	}
	list := p.bySender[from]
	for i, e := range list {
		if e.tx.Nonce() != tx.Nonce() {
			continue
		}
		min := new(big.Int).Mul(e.tx.GasPrice(), big.NewInt(110))
		min.Div(min, big.NewInt(100))
		if tx.GasPrice().Cmp(min) < 0 {
			return ErrUnderpriced
		}
		delete(p.byHash, e.tx.Hash())
		ne := &entry{tx: tx, from: from, seq: e.seq}
		list[i] = ne
		p.byHash[tx.Hash()] = ne
		return nil
	}
	if len(p.byHash) >= p.max {
		return ErrPoolFull
	}
	p.seq++
	e := &entry{tx: tx, from: from, seq: p.seq}
	p.byHash[tx.Hash()] = e
	list = append(list, e)
	sort.Slice(list, func(i, j int) bool { return list[i].tx.Nonce() < list[j].tx.Nonce() })
	p.bySender[from] = list
	return nil
}

// PendingNonce is the next nonce the sender can use: consecutive pooled nonces after stateNonce.
func (p *Pool) PendingNonce(a common.Address, stateNonce uint64) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := stateNonce
	for _, e := range p.bySender[a] {
		if e.tx.Nonce() == n {
			n++
		}
	}
	return n
}

// Select returns executable transactions (consecutive nonces from the state nonce) in arrival
// order, up to maxN transactions and gasLimit total gas.
func (p *Pool) Select(nonceOf func(common.Address) uint64, maxN int, gasLimit uint64) []*gtypes.Transaction {
	p.mu.Lock()
	defer p.mu.Unlock()
	ready := map[common.Address][]*entry{}
	for a, list := range p.bySender {
		n := nonceOf(a)
		for _, e := range list {
			if e.tx.Nonce() < n {
				continue
			}
			if e.tx.Nonce() != n {
				break
			}
			ready[a] = append(ready[a], e)
			n++
		}
	}
	var out []*gtypes.Transaction
	var gas uint64
	for len(out) < maxN {
		var best *entry
		var bestSender common.Address
		for a, l := range ready {
			if len(l) > 0 && (best == nil || l[0].seq < best.seq) {
				best, bestSender = l[0], a
			}
		}
		if best == nil || gas+best.tx.Gas() > gasLimit {
			break
		}
		out = append(out, best.tx)
		gas += best.tx.Gas()
		ready[bestSender] = ready[bestSender][1:]
	}
	return out
}

// Prune drops transactions whose nonce is already used (e.g. re-queued after a reorg that kept them).
func (p *Pool) Prune(nonceOf func(common.Address) uint64) {
	p.mu.Lock()
	var stale []common.Hash
	for a, list := range p.bySender {
		n := nonceOf(a)
		for _, e := range list {
			if e.tx.Nonce() < n {
				stale = append(stale, e.tx.Hash())
			}
		}
	}
	p.mu.Unlock()
	p.Remove(stale...)
}

// Remove drops transactions (included, skipped or invalid).
func (p *Pool) Remove(hashes ...common.Hash) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range hashes {
		e := p.byHash[h]
		if e == nil {
			continue
		}
		delete(p.byHash, h)
		list := p.bySender[e.from]
		for i, x := range list {
			if x == e {
				p.bySender[e.from] = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		if len(p.bySender[e.from]) == 0 {
			delete(p.bySender, e.from)
		}
	}
}

// Requeue puts transactions of dropped blocks back (no balance check: they were valid once and
// are re-validated when the next block is built).
func (p *Pool) Requeue(txs []*gtypes.Transaction, from func(*gtypes.Transaction) (common.Address, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tx := range txs {
		if p.byHash[tx.Hash()] != nil {
			continue
		}
		f, err := from(tx)
		if err != nil {
			continue
		}
		p.seq++
		e := &entry{tx: tx, from: f, seq: p.seq}
		p.byHash[tx.Hash()] = e
		l := append(p.bySender[f], e)
		sort.Slice(l, func(i, j int) bool { return l[i].tx.Nonce() < l[j].tx.Nonce() })
		p.bySender[f] = l
	}
}
