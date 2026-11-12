package derive_test

import (
	"crypto/ecdsa"
	"math/big"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/sergiiogg7/go-mini-rollup/internal/derive"
	"github.com/sergiiogg7/go-mini-rollup/internal/exec"
	"github.com/sergiiogg7/go-mini-rollup/internal/fakel1"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

const chainID = 901

var (
	eth      = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	inbox    = common.HexToAddress("0x00000000000000000000000000000000000ba7c4")
	bob      = common.HexToAddress("0x000000000000000000000000000000000000b0b0")
	carol    = common.HexToAddress("0x000000000000000000000000000000000000ca01")
	feeVault = common.HexToAddress("0x000000000000000000000000000000000000fee5")
)

func key(s string) *ecdsa.PrivateKey {
	k, err := crypto.HexToECDSA(s)
	if err != nil {
		panic(err)
	}
	return k
}

var (
	batcherKey = key("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	otherKey   = key("59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d")
	userKey    = key("5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdb9e0c0")
	userAddr   = crypto.PubkeyToAddress(userKey.PublicKey)
	batcher    = crypto.PubkeyToAddress(batcherKey.PublicKey)
)

type harness struct {
	t   *testing.T
	l1  *fakel1.FakeL1
	cfg *proto.Config
	ex  *exec.Executor
	d   *derive.Derivator
	reg *metrics.Registry
}

func baseConfig(forks ...proto.Fork) *proto.Config {
	if len(forks) == 0 {
		forks = []proto.Fork{{Activation: 0, Params: proto.V1(chainID)}}
	}
	return &proto.Config{
		L1Start: 1, Inbox: inbox, Batcher: batcher, FeeVault: feeVault,
		Alloc: map[common.Address]*big.Int{userAddr: new(big.Int).Mul(big.NewInt(100), eth)},
		Forks: forks,
	}
}

func newHarness(t *testing.T, forks ...proto.Fork) *harness {
	t.Helper()
	cfg := baseConfig(forks...)
	l1 := fakel1.New(inbox)
	return (&harness{t: t, l1: l1, cfg: cfg}).attach(l1)
}

func (h *harness) attach(l1 *fakel1.FakeL1) *harness {
	h.reg = metrics.NewRegistry()
	h.ex = exec.New(chainID, 30_000_000, feeVault)
	d, err := derive.New(h.cfg, h.ex, l1, h.reg)
	require.NoError(h.t, err)
	h.d = d
	return h
}

func (h *harness) tx(nonce uint64, to common.Address, v *big.Int) []byte {
	tx, err := gtypes.SignNewTx(userKey, gtypes.NewEIP155Signer(big.NewInt(chainID)),
		&gtypes.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1_000_000_000), Gas: 21000, To: &to, Value: v})
	require.NoError(h.t, err)
	raw, _ := tx.MarshalBinary()
	return raw
}

// epochTime is the L1 timestamp of block n in FakeL1.
func epochTime(n uint64) uint64 { return 1000 + 2*n }

func (h *harness) post(from *ecdsa.PrivateKey, b *proto.EpochBatch) {
	data, err := proto.EncodeBatch(b)
	require.NoError(h.t, err)
	_, err = h.l1.SendData(from, inbox, data)
	require.NoError(h.t, err)
}

func (h *harness) batch(epoch uint64, txs ...[]byte) *proto.EpochBatch {
	return &proto.EpochBatch{Epoch: epoch, Blocks: []proto.BatchBlock{{Time: epochTime(epoch) + 1, Txs: txs}}}
}

func (h *harness) sync() {
	h.t.Helper()
	require.NoError(h.t, h.d.SyncAll())
}

func (h *harness) balance(who common.Address) string {
	st, err := h.ex.StateAt(h.d.SafeHead().Header.Root)
	require.NoError(h.t, err)
	return st.GetBalance(who).ToBig().String()
}

func TestHappyPathDepositAndTransfer(t *testing.T) {
	h := newHarness(t)
	h.l1.QueueDeposit(bob, new(big.Int).Mul(big.NewInt(5), eth))
	h.l1.Mine(1)                                        // L1 block 1 = epoch 1, carries the deposit
	h.post(batcherKey, h.batch(1, h.tx(0, carol, eth))) // batch for epoch 1
	h.l1.Mine(1)                                        // L1 block 2 includes it
	h.sync()
	require.EqualValues(t, 1, h.d.SafeHead().Number())
	require.Equal(t, "5000000000000000000", h.balance(bob))
	require.Equal(t, "1000000000000000000", h.balance(carol))
	require.Equal(t, uint64(1), h.d.SafeHead().OriginNumber)
}

func TestSilentSequencerStillDerivesEmptyBlocksAndDeposits(t *testing.T) {
	h := newHarness(t)
	h.l1.QueueDeposit(bob, eth)
	h.l1.Mine(1)
	h.l1.Mine(11) // L1 head 12: epochs 1..4 reached epoch+SeqWindow(8)
	h.sync()
	require.EqualValues(t, 4, h.d.SafeHead().Number())
	require.Equal(t, "1000000000000000000", h.balance(bob), "deposits are forced even if the sequencer is silent")
	require.EqualValues(t, 4, h.reg.Get("rollup_fallback_epochs_total"))
}

func TestFirstValidBatchWinsDuplicatesIgnored(t *testing.T) {
	h := newHarness(t)
	h.l1.Mine(1)
	h.post(batcherKey, h.batch(1, h.tx(0, carol, eth)))
	h.l1.Mine(1)
	h.post(batcherKey, h.batch(1, h.tx(0, bob, eth))) // same epoch again, different content
	h.l1.Mine(1)
	h.sync()
	require.Equal(t, "1000000000000000000", h.balance(carol))
	require.Equal(t, "0", h.balance(bob))
	require.EqualValues(t, 1, h.reg.Get("rollup_batch_rejected_total", "reason", "stale_epoch"))
}

func bomb() []byte {
	// version 0 + zlib stream that inflates to 2 MiB of zeros (over the 1 MiB limit)
	b := &proto.EpochBatch{Epoch: 1, Blocks: []proto.BatchBlock{{Time: epochTime(1) + 1, Txs: [][]byte{make([]byte, 2<<20)}}}}
	d, _ := proto.EncodeBatch(b)
	return d
}

func TestAdversarialBatchesAreDroppedAndChainStaysLive(t *testing.T) {
	good := func(h *harness) *proto.EpochBatch { return h.batch(1, h.tx(0, carol, eth)) }
	cases := []struct {
		name, reason string
		run          func(h *harness)
	}{
		{"non-batcher sender", "sender", func(h *harness) { h.post(otherKey, good(h)) }},
		{"garbage bytes", "decode", func(h *harness) { _, _ = h.l1.SendData(batcherKey, inbox, []byte("not a batch at all")) }},
		{"decompression bomb", "decode", func(h *harness) { _, _ = h.l1.SendData(batcherKey, inbox, bomb()) }},
		{"future epoch", "future_epoch", func(h *harness) { h.post(batcherKey, h.batch(9, h.tx(0, carol, eth))) }},
		{"wrong version", "version", func(h *harness) { b := good(h); b.Version = 1; h.post(batcherKey, b) }},
		{"zero blocks", "block_count", func(h *harness) { h.post(batcherKey, &proto.EpochBatch{Epoch: 1}) }},
		{"too many blocks", "block_count", func(h *harness) {
			b := good(h)
			for i := 0; i < 20; i++ {
				b.Blocks = append(b.Blocks, proto.BatchBlock{Time: epochTime(1) + 2 + uint64(i)})
			}
			h.post(batcherKey, b)
		}},
		{"timestamp before epoch", "timestamps", func(h *harness) { b := good(h); b.Blocks[0].Time = epochTime(1) - 1; h.post(batcherKey, b) }},
		{"timestamp beyond drift", "timestamps", func(h *harness) { b := good(h); b.Blocks[0].Time = epochTime(1) + 601; h.post(batcherKey, b) }},
		{"non increasing timestamps", "timestamps", func(h *harness) {
			b := good(h)
			b.Blocks = append(b.Blocks, proto.BatchBlock{Time: b.Blocks[0].Time})
			h.post(batcherKey, b)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.l1.Mine(1)
			c.run(h)
			h.l1.Mine(10) // pass the sequencing window
			h.sync()
			require.EqualValues(t, 1, h.reg.Get("rollup_batch_rejected_total", "reason", c.reason), "reason %s", c.reason)
			require.Equal(t, "0", h.balance(carol), "the bad batch must not move funds")
			require.GreaterOrEqual(t, h.d.SafeHead().Number(), uint64(1), "chain must stay live (fallback)")
		})
	}
}

func TestInvalidTransactionsInsideValidBatchAreSkipped(t *testing.T) {
	h := newHarness(t)
	h.l1.Mine(1)
	h.post(batcherKey, h.batch(1, h.tx(0, carol, eth), h.tx(9, bob, eth), []byte{0x01, 0x02}, h.tx(1, carol, new(big.Int).Mul(big.NewInt(500), eth))))
	h.l1.Mine(1)
	h.sync()
	require.Equal(t, "1000000000000000000", h.balance(carol))
	require.Len(t, h.d.SafeBlock(1).Txs, 1)
	require.EqualValues(t, 1, h.reg.Get("rollup_tx_skipped_total", "reason", "undecodable"))
	require.EqualValues(t, 2, h.reg.Get("rollup_tx_skipped_total", "reason", "invalid"))
}

func TestReorgRemovesBatchThenDerivationConverges(t *testing.T) {
	h := newHarness(t)
	h.l1.Mine(1)
	h.post(batcherKey, h.batch(1, h.tx(0, carol, eth)))
	h.l1.Mine(1)
	h.sync()
	require.Equal(t, "1000000000000000000", h.balance(carol))
	h.l1.Reorg(1, false) // the block with the batch disappears
	h.sync()
	require.Equal(t, "0", h.balance(carol))
	require.GreaterOrEqual(t, h.d.Status().Reorgs, 1)
	h.l1.Reorg(1, true) // replay: the batch comes back in a different block
	h.post(batcherKey, h.batch(1, h.tx(0, carol, eth)))
	h.l1.Mine(1)
	h.sync()
	fresh := newHarness(t)
	fresh.cfg = h.cfg
	fresh.attach(h.l1)
	fresh.sync()
	require.Equal(t, fresh.d.SafeHead().Hash(), h.d.SafeHead().Hash())
}

// Property: any random script of mining, deposits, batches and reorgs, derived incrementally,
// ends in exactly the state of a fresh derivation of the final L1 chain.
func TestRandomReorgScriptsConvergeToFreshDerivation(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		h := newHarness(t)
		nonce := uint64(0)
		for step := 0; step < 40; step++ {
			switch rng.Intn(6) {
			case 0, 1:
				h.l1.Mine(1 + rng.Intn(2))
			case 2:
				h.l1.QueueDeposit(bob, big.NewInt(int64(1+rng.Intn(1000))))
			case 3:
				head, _ := h.l1.Head()
				if head >= 1 {
					e := 1 + uint64(rng.Intn(int(head)))
					h.post(batcherKey, h.batch(e, h.tx(nonce, carol, big.NewInt(1))))
					nonce++
				}
			case 4:
				head, _ := h.l1.Head()
				if head > 3 {
					h.l1.Reorg(1+rng.Intn(3), rng.Intn(2) == 0)
				}
			case 5:
				require.NoError(t, h.d.SyncAll())
			}
		}
		h.l1.Mine(12)
		require.NoError(t, h.d.SyncAll(), "seed %d", seed)
		fresh := &harness{t: t, l1: h.l1, cfg: h.cfg}
		fresh.attach(h.l1)
		require.NoError(t, fresh.d.SyncAll())
		require.Equal(t, fresh.d.SafeHead().Hash(), h.d.SafeHead().Hash(), "seed %d", seed)
		require.Equal(t, fresh.d.SafeHead().Number(), h.d.SafeHead().Number(), "seed %d", seed)
	}
}

func TestForkBoundaryAndOldNodeDiverges(t *testing.T) {
	forks := []proto.Fork{{Activation: 0, Params: proto.V1(chainID)}, {Activation: 5, Params: proto.V2(chainID)}}
	h := newHarness(t, forks...)
	h.l1.Mine(1)
	// pre-fork epochs use version 0 batches
	for e := uint64(1); e <= 7; e++ {
		if e > 1 {
			h.l1.Mine(1)
		}
		l1b, _ := h.l1.Block(e)
		b := h.batch(e)
		b.Epoch = e
		if e >= 5 {
			b.Version = 1
			b.OriginHash = l1b.Ref.Hash
			// V2 allows more than 16 blocks per epoch; V1 does not. Timestamps keep increasing across
			// epochs, so every epoch starts 25 s after the previous base.
			b.Blocks = nil
			for i := 0; i < 21; i++ {
				b.Blocks = append(b.Blocks, proto.BatchBlock{Time: epochTime(e) + 25*e + uint64(i)})
			}
		}
		h.post(batcherKey, b)
	}
	h.l1.Mine(2)
	h.sync()
	require.EqualValues(t, 0, h.reg.Get("rollup_batch_rejected_total", "reason", "version"))
	require.EqualValues(t, 4+3*21, h.d.SafeHead().Number(), "4 single-block V1 epochs plus 3 V2 epochs of 21 blocks")

	// an old node that never learned about the fork rejects the V2 batches and diverges
	old := newHarness(t)
	old.cfg = baseConfig() // V1 only
	old.attach(h.l1)
	old.sync()
	require.Greater(t, old.reg.Get("rollup_batch_rejected_total", "reason", "version"), float64(0))
	require.NotEqual(t, h.d.SafeHead().Hash(), old.d.SafeHead().Hash(), "old node must fork away at the activation epoch")
}

func TestWrongOriginHashAfterForkIsRejected(t *testing.T) {
	h := newHarness(t, proto.Fork{Activation: 0, Params: proto.V2(chainID)})
	h.l1.Mine(1)
	b := h.batch(1)
	b.Version = 1
	b.OriginHash = common.HexToHash("0xdead")
	h.post(batcherKey, b)
	h.l1.Mine(10)
	h.sync()
	require.EqualValues(t, 1, h.reg.Get("rollup_batch_rejected_total", "reason", "origin_hash"))
}

func TestFinalizedHeadLagsByFinalityDepth(t *testing.T) {
	h := newHarness(t)
	h.l1.Mine(1)
	h.post(batcherKey, h.batch(1, h.tx(0, carol, eth)))
	h.l1.Mine(1)
	h.sync()
	require.EqualValues(t, 1, h.d.Status().Safe)
	require.EqualValues(t, 0, h.d.Finalized())
	h.l1.Mine(12)
	h.sync()
	require.Greater(t, h.d.Finalized(), uint64(0))
}
