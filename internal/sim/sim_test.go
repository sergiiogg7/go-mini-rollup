// Package sim wires the whole rollup (fake L1, derivation, sequencer, batcher) and drives it round
// by round, so the end-to-end scenarios are deterministic and fast.
package sim

import (
	"crypto/ecdsa"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/sergiiogg7/go-mini-rollup/internal/batcher"
	"github.com/sergiiogg7/go-mini-rollup/internal/derive"
	"github.com/sergiiogg7/go-mini-rollup/internal/exec"
	"github.com/sergiiogg7/go-mini-rollup/internal/fakel1"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
	"github.com/sergiiogg7/go-mini-rollup/internal/sequencer"
)

const chainID = 901

var (
	eth      = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	inbox    = common.HexToAddress("0x00000000000000000000000000000000000ba7c4")
	bob      = common.HexToAddress("0x000000000000000000000000000000000000b0b0")
	carol    = common.HexToAddress("0x000000000000000000000000000000000000ca01")
	feeVault = common.HexToAddress("0x000000000000000000000000000000000000fee5")

	batcherKey = mustKey("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	userKey    = mustKey("5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdb9e0c0")
	userAddr   = crypto.PubkeyToAddress(userKey.PublicKey)
	deployCode = common.FromHex("0x600a600c600039600a6000f3602a60005260206000f3")
)

func mustKey(s string) *ecdsa.PrivateKey {
	k, err := crypto.HexToECDSA(s)
	if err != nil {
		panic(err)
	}
	return k
}

type node struct {
	t   *testing.T
	l1  *fakel1.FakeL1
	cfg *proto.Config
	ex  *exec.Executor
	der *derive.Derivator
	seq *sequencer.Sequencer
	bat *batcher.Batcher
	reg *metrics.Registry
}

func newNode(t *testing.T, forks ...proto.Fork) *node {
	cfg := &proto.Config{
		L1Start: 1, Inbox: inbox, Batcher: crypto.PubkeyToAddress(batcherKey.PublicKey), FeeVault: feeVault,
		Alloc: map[common.Address]*big.Int{userAddr: new(big.Int).Mul(big.NewInt(100), eth)},
		Forks: []proto.Fork{{Activation: 0, Params: proto.V1(chainID)}},
	}
	if len(forks) > 0 {
		cfg.Forks = forks
	}
	n := &node{t: t, l1: fakel1.New(inbox), cfg: cfg}
	n.boot()
	return n
}

// boot builds fresh components over the same L1 (also used to simulate a restart).
func (n *node) boot() {
	n.reg = metrics.NewRegistry()
	n.ex = exec.New(chainID, 30_000_000, feeVault)
	var err error
	n.der, err = derive.New(n.cfg, n.ex, n.l1, n.reg)
	require.NoError(n.t, err)
	n.seq, err = sequencer.New(n.cfg, n.ex, n.l1, n.der, n.reg)
	require.NoError(n.t, err)
	n.seq.Now = func() uint64 { h, _ := n.l1.Head(); return 1000 + 2*h + 1 }
	n.bat = batcher.New(n.cfg, n.seq, n.l1, n.l1, batcherKey, n.reg)
}

// round: mine one L1 block, then every component takes one step.
func (n *node) round() { n.roundWith(true) }

func (n *node) roundWith(batcherUp bool) {
	n.l1.Mine(1)
	n.steps(batcherUp)
}

func (n *node) steps(batcherUp bool) {
	_, err := n.seq.Tick()
	require.NoError(n.t, err)
	if batcherUp {
		require.NoError(n.t, n.bat.Tick())
	}
	require.NoError(n.t, n.der.SyncAll())
	_, err = n.seq.Tick() // let the sequencer see the new canonical chain
	require.NoError(n.t, err)
}

func (n *node) rounds(k int) {
	for i := 0; i < k; i++ {
		n.round()
	}
}

func (n *node) tx(to *common.Address, v *big.Int, data []byte, gas uint64) (common.Hash, []byte) {
	nonce := n.seq.PendingNonce(userAddr)
	tx, err := gtypes.SignNewTx(userKey, gtypes.NewEIP155Signer(big.NewInt(chainID)),
		&gtypes.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1_000_000_000), Gas: gas, To: to, Value: v, Data: data})
	require.NoError(n.t, err)
	raw, _ := tx.MarshalBinary()
	return tx.Hash(), raw
}

func (n *node) send(to common.Address, v *big.Int) common.Hash {
	h, raw := n.tx(&to, v, nil, 21000)
	_, err := n.seq.Submit(raw)
	require.NoError(n.t, err)
	return h
}

func (n *node) balanceAt(b *proto.Block, a common.Address) string {
	st, err := n.ex.StateAt(b.Header.Root)
	require.NoError(n.t, err)
	return st.GetBalance(a).ToBig().String()
}

// converged asserts the sequencer's chain equals the canonical (derived) chain block by block.
func (n *node) converged() {
	n.t.Helper()
	safe := n.der.SafeHead().Number()
	for i := uint64(0); i <= safe; i++ {
		require.Equal(n.t, n.der.SafeBlock(i).Hash(), n.seq.Block(i).Hash(), "block %d differs between sequencer and derivation", i)
	}
}

func TestDepositTransferAndConvergence(t *testing.T) {
	n := newNode(t)
	n.l1.QueueDeposit(bob, new(big.Int).Mul(big.NewInt(5), eth))
	n.rounds(2)
	h := n.send(carol, eth)
	n.rounds(6)
	loc, ok := n.seq.Tx(h)
	require.True(t, ok, "transaction included in the unsafe chain")
	require.EqualValues(t, 1, loc.Block.Receipts[loc.Index].Status)
	n.converged()
	safe := n.der.SafeHead()
	require.Equal(t, "5000000000000000000", n.balanceAt(safe, bob), "deposit minted on L2 without trusting the sequencer")
	require.Equal(t, "1000000000000000000", n.balanceAt(safe, carol))
	require.Equal(t, uint64(0), uint64(n.seq.UnsafeReorgs()))
}

func TestContractDeployAndCallThroughTheWholePipeline(t *testing.T) {
	n := newNode(t)
	n.rounds(2)
	h, raw := n.tx(nil, new(big.Int), deployCode, 200_000)
	_, err := n.seq.Submit(raw)
	require.NoError(t, err)
	n.rounds(6)
	loc, ok := n.seq.Tx(h)
	require.True(t, ok)
	contract := loc.Block.Receipts[loc.Index].ContractAddress
	require.Equal(t, crypto.CreateAddress(userAddr, 0), contract)
	n.converged()
	res, err := n.ex.Call(n.der.SafeHead().Header.Root, 99, 9999, exec.CallMsg{From: bob, To: &contract})
	require.NoError(t, err)
	require.Equal(t, common.LeftPadBytes([]byte{42}, 32), res.ReturnData)
}

func TestL1ReorgDropsTheBatchAndTheBatcherPostsItAgain(t *testing.T) {
	n := newNode(t)
	n.rounds(2)
	h := n.send(carol, eth)
	n.rounds(4)
	n.converged()
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), carol))
	n.l1.Reorg(3, false) // the blocks that held the batches disappear
	n.steps(true)
	n.rounds(8)
	n.converged()
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), carol), "the transfer survives the reorg")
	_, ok := n.seq.Tx(h)
	require.True(t, ok)
	require.Greater(t, n.reg.Get("rollup_batcher_resubmitted_total", "cause", "l1_reorg"), float64(0))
}

func TestUnsafeChainFollowsAnL1OriginReorg(t *testing.T) {
	n := newNode(t)
	n.rounds(3)
	h := n.send(carol, eth)
	n.l1.Mine(1) // origin block that the sequencer will build on
	n.steps(false)
	_, ok := n.seq.Tx(h)
	require.True(t, ok)
	n.l1.Reorg(2, false) // that origin disappears before any batch was posted
	n.steps(false)
	require.Greater(t, n.seq.UnsafeReorgs(), 0, "unsafe blocks on a vanished origin are dropped")
	n.rounds(8)
	n.converged()
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), carol), "the dropped transaction was re-queued and included again")
}

func TestDerivationOverridesTheSequencerAndTxsAreRequeued(t *testing.T) {
	n := newNode(t)
	n.rounds(4) // L1 head 4: the sequencer is filling epoch 3
	h := n.send(carol, eth)
	n.steps(false) // the transaction is now in an unsafe block of epoch 3
	_, ok := n.seq.Tx(h)
	require.True(t, ok)
	// a batcher that posts a different batch than the sequencer built: the canonical chain wins
	rogue := &proto.EpochBatch{Epoch: 3, Blocks: []proto.BatchBlock{{Time: 1000 + 2*3 + 1}}}
	data, err := proto.EncodeBatch(rogue)
	require.NoError(t, err)
	_, err = n.l1.SendData(batcherKey, inbox, data)
	require.NoError(t, err)
	n.rounds(10)
	n.converged()
	require.Greater(t, n.seq.UnsafeReorgs(), 0, "sequencer had to reorg onto the canonical chain")
	loc, ok := n.seq.Tx(h)
	require.True(t, ok, "the transaction was not lost: it was re-queued and included in a later epoch")
	require.Greater(t, loc.Block.OriginNumber, uint64(3))
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), carol))
}

func TestBatcherOutageLongerThanTheWindowDoesNotLoseTransactions(t *testing.T) {
	n := newNode(t)
	n.rounds(2)
	h := n.send(carol, eth)
	for i := 0; i < 14; i++ { // batcher down for more than SeqWindow (8) blocks
		n.roundWith(false)
	}
	require.EqualValues(t, 0, n.reg.Get("rollup_batch_accepted_total"))
	require.Greater(t, n.reg.Get("rollup_fallback_epochs_total"), float64(0), "chain stayed live through empty fallback blocks")
	for i := 0; i < 10; i++ { // batcher back
		n.round()
	}
	n.converged()
	_, ok := n.seq.Tx(h)
	require.True(t, ok)
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), carol))
}

func TestStuckBatchGetsAFeeBump(t *testing.T) {
	n := newNode(t)
	n.rounds(2)
	n.send(carol, eth)
	n.l1.Mine(1)
	_, _ = n.seq.Tick()
	n.l1.Mine(1)
	_, _ = n.seq.Tick()
	// batcher submits but L1 stops mining: the batch sits in the pool
	require.NoError(t, n.bat.Tick())
	for i := 0; i < 6; i++ {
		require.NoError(t, n.bat.Tick())
	}
	require.Greater(t, n.reg.Get("rollup_batcher_resubmitted_total", "cause", "fee_bump"), float64(0))
	n.rounds(6)
	n.converged()
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), carol))
}

func TestRestartRebuildsTheSameChainAndDoesNotRepostBatches(t *testing.T) {
	n := newNode(t)
	n.l1.QueueDeposit(bob, eth)
	n.rounds(2)
	n.send(carol, eth)
	n.rounds(8)
	before := n.der.SafeHead().Hash()
	posted := n.reg.Get("rollup_batch_submitted_total")
	require.Greater(t, posted, float64(0))
	n.boot() // every component restarts with no local state
	n.rounds(1)
	require.GreaterOrEqual(t, n.der.SafeHead().Number(), uint64(1))
	n.rounds(6)
	// the new batcher recovered its position from L1 and did not post the old epochs again
	require.Less(t, n.reg.Get("rollup_batch_submitted_total"), posted)
	// a freshly derived node agrees on the canonical history it had before
	fresh := n.der.SafeBlock(1)
	require.NotNil(t, fresh)
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), bob))
	require.NotEqual(t, common.Hash{}, before)
}

func TestForkActivatesAcrossTheWholePipeline(t *testing.T) {
	n := newNode(t, proto.Fork{Activation: 0, Params: proto.V1(chainID)}, proto.Fork{Activation: 6, Params: proto.V2(chainID)})
	n.rounds(4)
	h1 := n.send(carol, eth)
	n.rounds(6) // crosses epoch 6
	h2 := n.send(bob, eth)
	n.rounds(8)
	n.converged()
	for _, h := range []common.Hash{h1, h2} {
		_, ok := n.seq.Tx(h)
		require.True(t, ok)
	}
	require.EqualValues(t, 0, n.reg.Get("rollup_batch_rejected_total", "reason", "version"), "batcher switched format exactly at the fork")
	require.Equal(t, "1000000000000000000", n.balanceAt(n.der.SafeHead(), bob))
}

func TestBlockBuildAndPoolMetrics(t *testing.T) {
	n := newNode(t)
	metrics.RegisterAll(n.reg)
	n.rounds(2)
	h, raw := n.tx(&carol, eth, nil, 21000)
	_, err := n.seq.Submit(raw)
	require.NoError(t, err)
	_, err = n.seq.Submit(raw) // same transaction again
	require.Error(t, err)
	require.EqualValues(t, 1, n.reg.Get(metrics.TxRejected, "reason", "known"), "bounded label, not the error text")
	require.EqualValues(t, 1, n.reg.Get(metrics.TxPoolSize))
	n.rounds(6)
	_, ok := n.seq.Tx(h)
	require.True(t, ok)
	require.EqualValues(t, 0, n.reg.Get(metrics.TxPoolSize))
	require.Greater(t, n.reg.Count(metrics.BlockBuild), uint64(0))
	require.Greater(t, n.reg.Get(metrics.BlocksBuilt, "kind", "tx"), float64(0))
	require.Contains(t, n.reg.Render(), "rollup_block_build_seconds_bucket{le=\"+Inf\"}")
	require.EqualValues(t, 0, n.reg.Get(metrics.FallbackEpochs), "counter exported at 0 before the first event")
}

func TestBatcherReportsInflightAgeWhileL1DoesNotConfirm(t *testing.T) {
	n := newNode(t)
	clock := time.Unix(1_700_000_000, 0)
	n.bat.Now = func() time.Time { return clock }
	n.rounds(2)
	n.send(carol, eth)
	n.l1.Mine(1)
	_, _ = n.seq.Tick()
	n.l1.Mine(1)
	_, _ = n.seq.Tick()
	require.NoError(t, n.bat.Tick()) // sent, L1 stops mining: it stays in the pool
	require.EqualValues(t, 0, n.reg.Get(metrics.BatcherOldestAge))
	require.Greater(t, n.reg.Get(metrics.BatcherInflight), float64(0))
	clock = clock.Add(90 * time.Second)
	require.NoError(t, n.bat.Tick())
	require.EqualValues(t, 90, n.reg.Get(metrics.BatcherOldestAge))
	n.l1.Mine(1) // the batches get included
	require.NoError(t, n.bat.Tick())
	require.EqualValues(t, 0, n.reg.Get(metrics.BatcherOldestAge))
	require.EqualValues(t, 0, n.reg.Get(metrics.BatcherInflight))
}

func TestFeeBumpsStopAtTheGasPriceCap(t *testing.T) {
	n := newNode(t)
	n.bat.MaxGasPrice = big.NewInt(1_500_000_000) // the fake L1 quotes 1 gwei: 1 -> 1.25 -> cap
	n.rounds(2)
	n.send(carol, eth)
	n.l1.Mine(1)
	_, _ = n.seq.Tick()
	n.l1.Mine(1)
	_, _ = n.seq.Tick()
	for i := 0; i < 60; i++ { // L1 stops mining: the batches stay stuck
		require.NoError(t, n.bat.Tick(), "no error once the cap is reached")
	}
	bumps := n.reg.Get(metrics.BatcherResubmit, "cause", "fee_bump")
	require.Greater(t, bumps, float64(0))
	require.LessOrEqual(t, bumps, float64(2*n.bat.MaxInflight), "bounded: each in-flight batch is bumped at most twice")
}
