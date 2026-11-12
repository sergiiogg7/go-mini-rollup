package rpcapi_test

import (
	"context"
	"errors"
	"math/big"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/sergiiogg7/go-mini-rollup/internal/batcher"
	"github.com/sergiiogg7/go-mini-rollup/internal/derive"
	"github.com/sergiiogg7/go-mini-rollup/internal/exec"
	"github.com/sergiiogg7/go-mini-rollup/internal/fakel1"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
	"github.com/sergiiogg7/go-mini-rollup/internal/rpcapi"
	"github.com/sergiiogg7/go-mini-rollup/internal/sequencer"
)

const chainID = 901

var (
	eth      = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	inbox    = common.HexToAddress("0x00000000000000000000000000000000000ba7c4")
	feeVault = common.HexToAddress("0x000000000000000000000000000000000000fee5")
	bob      = common.HexToAddress("0x000000000000000000000000000000000000b0b0")
	carol    = common.HexToAddress("0x000000000000000000000000000000000000ca01")

	batcherKey, _ = crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	userKey, _    = crypto.HexToECDSA("5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdb9e0c0")
	user          = crypto.PubkeyToAddress(userKey.PublicKey)

	// init code of a contract whose runtime returns the 32-byte word 42 for any call
	deployAnswer = common.FromHex("0x600a600c600039600a6000f3602a60005260206000f3")
	// init code of a contract whose runtime is LOG1(topic=0xaa, empty data) and stops
	deployLogger = common.FromHex("0x6007600c60003960076000f360aa60006000a1")
	// init code of a contract whose runtime reverts with empty data
	deployReverter = common.FromHex("0x6005600c60003960056000f360006000fd")
)

// env is the whole rollup behind an HTTP JSON-RPC server, driven round by round like internal/sim.
type env struct {
	t   *testing.T
	l1  *fakel1.FakeL1
	der *derive.Derivator
	seq *sequencer.Sequencer
	bat *batcher.Batcher
	reg *metrics.Registry

	seqRPC, verRPC *rpc.Client
	seqEth, verEth *ethclient.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := &proto.Config{
		L1Start: 1, Inbox: inbox, Batcher: crypto.PubkeyToAddress(batcherKey.PublicKey), FeeVault: feeVault,
		Alloc: map[common.Address]*big.Int{user: new(big.Int).Mul(big.NewInt(100), eth)},
		Forks: []proto.Fork{{Activation: 0, Params: proto.V1(chainID)}},
	}
	e := &env{t: t, l1: fakel1.New(inbox), reg: metrics.NewRegistry()}
	ex := exec.New(chainID, 30_000_000, feeVault)
	var err error
	e.der, err = derive.New(cfg, ex, e.l1, e.reg)
	require.NoError(t, err)
	e.seq, err = sequencer.New(cfg, ex, e.l1, e.der, e.reg)
	require.NoError(t, err)
	e.seq.Now = func() uint64 { h, _ := e.l1.Head(); return 1000 + 2*h + 1 }
	e.bat = batcher.New(cfg, e.seq, e.l1, e.l1, batcherKey, e.reg)

	serve := func(chain rpcapi.Chain) (*rpc.Client, *ethclient.Client) {
		h, err := rpcapi.New(chain, ex, chainID, "go-mini-rollup/test")
		require.NoError(t, err)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		c, err := rpc.DialHTTP(srv.URL)
		require.NoError(t, err)
		t.Cleanup(c.Close)
		return c, ethclient.NewClient(c)
	}
	e.seqRPC, e.seqEth = serve(&rpcapi.SeqChain{Seq: e.seq, Der: e.der, L1: e.l1, Bat: e.bat, Reg: e.reg})
	e.verRPC, e.verEth = serve(&rpcapi.VerifierChain{Der: e.der, L1: e.l1})
	return e
}

func (e *env) round() {
	e.t.Helper()
	e.l1.Mine(1)
	_, err := e.seq.Tick()
	require.NoError(e.t, err)
	require.NoError(e.t, e.bat.Tick())
	require.NoError(e.t, e.der.SyncAll())
	_, err = e.seq.Tick()
	require.NoError(e.t, err)
}

func (e *env) rounds(n int) {
	for i := 0; i < n; i++ {
		e.round()
	}
}

func (e *env) signed(nonce uint64, to *common.Address, value *big.Int, data []byte, gas uint64) *gtypes.Transaction {
	e.t.Helper()
	tx, err := gtypes.SignNewTx(userKey, gtypes.NewEIP155Signer(big.NewInt(chainID)),
		&gtypes.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1_000_000_000), Gas: gas, To: to, Value: value, Data: data})
	require.NoError(e.t, err)
	return tx
}

func (e *env) send(tx *gtypes.Transaction) {
	e.t.Helper()
	require.NoError(e.t, e.seqEth.SendTransaction(context.Background(), tx))
}

// deploy sends a contract creation and mines it; it returns the contract address.
func (e *env) deploy(nonce uint64, code []byte) common.Address {
	e.t.Helper()
	tx := e.signed(nonce, nil, new(big.Int), code, 200_000)
	e.send(tx)
	e.rounds(3)
	rc, err := e.seqEth.TransactionReceipt(context.Background(), tx.Hash())
	require.NoError(e.t, err)
	require.EqualValues(e.t, 1, rc.Status)
	return rc.ContractAddress
}

var bg = context.Background()

func TestIdentityAndStaticMethods(t *testing.T) {
	e := newEnv(t)
	id, err := e.seqEth.ChainID(bg)
	require.NoError(t, err)
	require.EqualValues(t, chainID, id.Int64())

	var s string
	require.NoError(t, e.seqRPC.Call(&s, "net_version"))
	require.Equal(t, "901", s)
	require.NoError(t, e.seqRPC.Call(&s, "web3_clientVersion"))
	require.Equal(t, "go-mini-rollup/test", s)
	var b bool
	require.NoError(t, e.seqRPC.Call(&b, "net_listening"))
	require.True(t, b)
	require.NoError(t, e.seqRPC.Call(&b, "eth_mining"))
	require.False(t, b)
	sync, err := e.seqEth.SyncProgress(bg)
	require.NoError(t, err)
	require.Nil(t, sync, "eth_syncing=false means not syncing")
	price, err := e.seqEth.SuggestGasPrice(bg)
	require.NoError(t, err)
	require.EqualValues(t, 1_000_000_000, price.Int64())
	var accounts []common.Address
	require.NoError(t, e.seqRPC.Call(&accounts, "eth_accounts"))
	require.Empty(t, accounts, "a node with no wallet")
	fh, err := e.seqEth.FeeHistory(bg, 2, nil, []float64{50})
	require.NoError(t, err)
	require.Len(t, fh.BaseFee, 2, "only the genesis block exists: the count is clipped to 1, plus the next block")
}

func TestBalancesAtEveryBlockSelector(t *testing.T) {
	e := newEnv(t)
	e.l1.QueueDeposit(bob, new(big.Int).Mul(big.NewInt(5), eth))
	e.rounds(8) // the safe head needs the batch of the epoch to be included on L1
	require.GreaterOrEqual(t, e.der.SafeHead().Number(), uint64(1), "the deposit was derived into block 1")

	bal := func(sel string) string {
		var out hexutil.Big
		require.NoError(t, e.seqRPC.Call(&out, "eth_getBalance", bob, sel))
		return out.ToInt().String()
	}
	five := "5000000000000000000"
	require.Equal(t, five, bal("latest"))
	require.Equal(t, five, bal("safe"))
	require.Equal(t, five, bal("pending"))
	require.Equal(t, "0", bal("earliest"), "genesis precedes the deposit")
	require.Equal(t, five, bal("0x1"))
	got, err := e.seqEth.BalanceAt(bg, bob, big.NewInt(1))
	require.NoError(t, err)
	require.Equal(t, five, got.String())

	h := e.der.SafeBlock(1).Hash()
	var out hexutil.Big
	require.NoError(t, e.seqRPC.Call(&out, "eth_getBalance", bob, map[string]any{"blockHash": h}))
	require.Equal(t, five, out.ToInt().String(), "EIP-1898 block hash selector")

	err = e.seqRPC.Call(&out, "eth_getBalance", bob, "0x999")
	require.ErrorContains(t, err, "block not found")
	err = e.seqRPC.Call(&out, "eth_getBalance", bob, map[string]any{"blockHash": common.Hash{1}})
	require.ErrorContains(t, err, "header for hash not found")
	require.Equal(t, "0", bal("finalized"), "nothing is finalized yet: the genesis state")
}

func TestNonceAndPendingNonceAcrossTheLifeOfATransaction(t *testing.T) {
	e := newEnv(t)
	e.rounds(2)
	nonceAt := func(sel string) uint64 {
		var n hexutil.Uint64
		require.NoError(t, e.seqRPC.Call(&n, "eth_getTransactionCount", user, sel))
		return uint64(n)
	}
	require.EqualValues(t, 0, nonceAt("latest"))

	tx := e.signed(0, &carol, eth, nil, 21000)
	e.send(tx)
	require.EqualValues(t, 0, nonceAt("latest"), "not in a block yet")
	require.EqualValues(t, 1, nonceAt("pending"), "wallets use the pending nonce to queue the next transaction")
	pn, err := e.seqEth.PendingNonceAt(bg, user)
	require.NoError(t, err)
	require.EqualValues(t, 1, pn)

	// pending lookups: the transaction is visible with null block fields, and has no receipt
	var raw map[string]any
	require.NoError(t, e.seqRPC.Call(&raw, "eth_getTransactionByHash", tx.Hash()))
	require.NotNil(t, raw, "a pending transaction can be fetched by hash")
	require.Nil(t, raw["blockHash"])
	require.Nil(t, raw["blockNumber"])
	_, err = e.seqEth.TransactionReceipt(bg, tx.Hash())
	require.ErrorIs(t, err, ethereum.NotFound)

	e.rounds(3)
	require.EqualValues(t, 1, nonceAt("latest"))
	rc, err := e.seqEth.TransactionReceipt(bg, tx.Hash())
	require.NoError(t, err)
	require.EqualValues(t, 1, rc.Status)
	require.EqualValues(t, 21000, rc.GasUsed)
	require.Equal(t, tx.Hash(), rc.TxHash)
	require.Equal(t, common.Address{}, rc.ContractAddress)
}

func TestSendRawTransactionErrorShapes(t *testing.T) {
	e := newEnv(t)
	e.rounds(2)
	good := e.signed(0, &carol, eth, nil, 21000)
	e.send(good)

	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"duplicate", must(good.MarshalBinary()), "already known"},
		{"garbage", []byte{0x01, 0x02, 0x03}, "decode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h common.Hash
			err := e.seqRPC.Call(&h, "eth_sendRawTransaction", hexutil.Bytes(c.raw))
			require.ErrorContains(t, err, c.want)
			var re rpc.Error
			require.True(t, errors.As(err, &re), "a JSON-RPC error object, not a transport error")
		})
	}
	e.rounds(3) // include it, then reuse nonce 0
	t.Run("nonce too low", func(t *testing.T) {
		tx := e.signed(0, &bob, big.NewInt(1), nil, 21000)
		require.ErrorContains(t, e.seqEth.SendTransaction(bg, tx), "nonce too low")
	})
	t.Run("nonce gap too far ahead", func(t *testing.T) {
		tx := e.signed(1000, &bob, big.NewInt(1), nil, 21000)
		require.ErrorContains(t, e.seqEth.SendTransaction(bg, tx), "nonce too far")
	})
	t.Run("insufficient funds", func(t *testing.T) {
		tx := e.signed(1, &bob, new(big.Int).Mul(big.NewInt(1000), eth), nil, 21000)
		require.ErrorContains(t, e.seqEth.SendTransaction(bg, tx), "insufficient funds")
	})
	t.Run("not replay protected", func(t *testing.T) {
		tx, err := gtypes.SignNewTx(userKey, gtypes.HomesteadSigner{}, &gtypes.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1), Gas: 21000, To: &bob})
		require.NoError(t, err)
		require.ErrorContains(t, e.seqEth.SendTransaction(bg, tx), "replay-protected")
	})
	t.Run("wrong chain id", func(t *testing.T) {
		tx, err := gtypes.SignNewTx(userKey, gtypes.NewEIP155Signer(big.NewInt(1)), &gtypes.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1), Gas: 21000, To: &bob})
		require.NoError(t, err)
		require.Error(t, e.seqEth.SendTransaction(bg, tx))
	})
	t.Run("gas above the block gas limit", func(t *testing.T) {
		tx := e.signed(1, &bob, big.NewInt(1), nil, 40_000_000)
		require.ErrorContains(t, e.seqEth.SendTransaction(bg, tx), "block gas limit")
	})
	t.Run("blob transactions", func(t *testing.T) {
		tx, err := gtypes.SignNewTx(userKey, gtypes.NewCancunSigner(big.NewInt(chainID)), &gtypes.BlobTx{
			ChainID: uint256.NewInt(chainID), Nonce: 1, GasFeeCap: uint256.NewInt(1), GasTipCap: uint256.NewInt(1), Gas: 21000, To: bob,
		})
		require.NoError(t, err)
		require.ErrorContains(t, e.seqEth.SendTransaction(bg, tx), "blob")
	})
	t.Run("too large", func(t *testing.T) {
		tx := e.signed(1, &bob, big.NewInt(0), make([]byte, 200*1024), 21000)
		require.ErrorContains(t, e.seqEth.SendTransaction(bg, tx), "too large")
	})
}

func TestContractDeploymentCallEstimateAndRevertShape(t *testing.T) {
	e := newEnv(t)
	e.rounds(2)
	answer := e.deploy(0, deployAnswer)
	require.NotEqual(t, common.Address{}, answer)

	code, err := e.seqEth.CodeAt(bg, answer, nil)
	require.NoError(t, err)
	require.NotEmpty(t, code)
	out, err := e.seqEth.CallContract(bg, ethereum.CallMsg{To: &answer}, nil)
	require.NoError(t, err)
	require.Equal(t, common.LeftPadBytes([]byte{42}, 32), out)

	// the same call with `input` instead of `data`, and from a funded sender
	var raw hexutil.Bytes
	require.NoError(t, e.seqRPC.Call(&raw, "eth_call", map[string]any{"to": answer, "input": "0x", "from": user}, "latest"))
	require.Equal(t, common.LeftPadBytes([]byte{42}, 32), []byte(raw))

	gas, err := e.seqEth.EstimateGas(bg, ethereum.CallMsg{From: user, To: &carol, Value: big.NewInt(1)})
	require.NoError(t, err)
	require.EqualValues(t, 21000, gas, "a plain transfer costs exactly 21000")
	gas, err = e.seqEth.EstimateGas(bg, ethereum.CallMsg{From: user, To: &answer})
	require.NoError(t, err)
	require.Greater(t, gas, uint64(21000))

	slot, err := e.seqEth.StorageAt(bg, answer, common.Hash{}, nil)
	require.NoError(t, err)
	require.Len(t, slot, 32)

	reverter := e.deploy(1, deployReverter)
	err = e.seqRPC.Call(&raw, "eth_call", map[string]any{"to": reverter}, "latest")
	require.ErrorContains(t, err, "execution reverted")
	var de rpc.DataError
	require.True(t, errors.As(err, &de), "revert data travels in error.data, as wallets expect")
	var ce rpc.Error
	require.True(t, errors.As(err, &ce))
	require.Equal(t, 3, ce.ErrorCode(), "code 3 is what cast and ethers decode as a revert")
	_, err = e.seqEth.EstimateGas(bg, ethereum.CallMsg{From: user, To: &reverter})
	require.ErrorContains(t, err, "execution reverted")
}

func TestLogsFilteredByAddressTopicRangeAndBlockHash(t *testing.T) {
	e := newEnv(t)
	e.rounds(2)
	logger := e.deploy(0, deployLogger)
	other := e.deploy(1, deployLogger)
	for i, to := range []common.Address{logger, other, logger} {
		e.send(e.signed(uint64(2+i), &to, new(big.Int), nil, 100_000))
	}
	e.rounds(4)
	topic := common.BigToHash(big.NewInt(0xaa))

	logs, err := e.seqEth.FilterLogs(bg, ethereum.FilterQuery{FromBlock: big.NewInt(0), Addresses: []common.Address{logger}})
	require.NoError(t, err)
	require.Len(t, logs, 2, "two calls to the first logger")
	for _, l := range logs {
		require.Equal(t, logger, l.Address)
		require.Equal(t, []common.Hash{topic}, l.Topics)
		require.False(t, l.Removed)
	}
	all, err := e.seqEth.FilterLogs(bg, ethereum.FilterQuery{FromBlock: big.NewInt(0), Topics: [][]common.Hash{{topic}}})
	require.NoError(t, err)
	require.Len(t, all, 3)
	none, err := e.seqEth.FilterLogs(bg, ethereum.FilterQuery{FromBlock: big.NewInt(0), Topics: [][]common.Hash{{common.Hash{9}}}})
	require.NoError(t, err)
	require.Empty(t, none, "an empty result is [], never null")

	byHash, err := e.seqEth.FilterLogs(bg, ethereum.FilterQuery{BlockHash: &logs[0].BlockHash})
	require.NoError(t, err)
	require.NotEmpty(t, byHash)
	for _, l := range byHash {
		require.Equal(t, logs[0].BlockHash, l.BlockHash)
	}
	narrow, err := e.seqEth.FilterLogs(bg, ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(logs[1].BlockNumber), ToBlock: new(big.Int).SetUint64(logs[1].BlockNumber), Addresses: []common.Address{logger}})
	require.NoError(t, err)
	inBlock := 0
	for _, l := range logs {
		if l.BlockNumber == logs[1].BlockNumber {
			inBlock++
		}
	}
	require.Len(t, narrow, inBlock, "only the logs of that block")
	require.Equal(t, logs[1].BlockNumber, narrow[0].BlockNumber)

	// the receipt carries the same log
	rc, err := e.seqEth.TransactionReceipt(bg, logs[0].TxHash)
	require.NoError(t, err)
	require.Len(t, rc.Logs, 1)
}

func TestBlocksAndTransactionsByNumberAndHash(t *testing.T) {
	e := newEnv(t)
	e.rounds(2)
	tx := e.signed(0, &carol, eth, nil, 21000)
	e.send(tx)
	e.rounds(3)

	head, err := e.seqEth.BlockNumber(bg)
	require.NoError(t, err)
	require.Greater(t, head, uint64(1))
	var found *gtypes.Block
	for n := uint64(0); n <= head; n++ {
		b, err := e.seqEth.BlockByNumber(bg, new(big.Int).SetUint64(n))
		require.NoError(t, err)
		require.Equal(t, n, b.NumberU64())
		if b.Transaction(tx.Hash()) != nil {
			found = b
		}
	}
	require.NotNil(t, found, "the transfer is in one block, with full transaction bodies")
	byHash, err := e.seqEth.BlockByHash(bg, found.Hash())
	require.NoError(t, err)
	require.Equal(t, found.Hash(), byHash.Hash())

	var raw map[string]any
	require.NoError(t, e.seqRPC.Call(&raw, "eth_getBlockByNumber", hexutil.Uint64(found.NumberU64()), false))
	require.Contains(t, raw, "l1OriginNumber", "rollup extension: the L1 origin of the block")
	require.Equal(t, "0x0", raw["baseFeePerGas"])

	_, err = e.seqEth.BlockByNumber(bg, big.NewInt(99999))
	require.ErrorIs(t, err, ethereum.NotFound)
	_, err = e.seqEth.BlockByHash(bg, common.Hash{7})
	require.ErrorIs(t, err, ethereum.NotFound)
	got, pending, err := e.seqEth.TransactionByHash(bg, tx.Hash())
	require.NoError(t, err)
	require.False(t, pending)
	require.Equal(t, tx.Hash(), got.Hash())
	_, _, err = e.seqEth.TransactionByHash(bg, common.Hash{7})
	require.ErrorIs(t, err, ethereum.NotFound)
}

func TestBatchRequestsAnswerEveryElementIndependently(t *testing.T) {
	e := newEnv(t)
	e.rounds(2)
	var (
		chain    hexutil.Big
		number   hexutil.Uint64
		balance  hexutil.Big
		bad      hexutil.Big
		unknown  string
		badParam hexutil.Big
	)
	batch := []rpc.BatchElem{
		{Method: "eth_chainId", Result: &chain},
		{Method: "eth_blockNumber", Result: &number},
		{Method: "eth_getBalance", Args: []any{user, "latest"}, Result: &balance},
		{Method: "eth_getBalance", Args: []any{user, "0xfffff"}, Result: &bad},
		{Method: "eth_doesNotExist", Result: &unknown},
		{Method: "eth_getBalance", Args: []any{"not-an-address", "latest"}, Result: &badParam},
	}
	require.NoError(t, e.seqRPC.BatchCall(batch), "the HTTP exchange succeeds even when elements fail")
	require.NoError(t, batch[0].Error)
	require.EqualValues(t, chainID, chain.ToInt().Int64())
	require.NoError(t, batch[1].Error)
	require.NoError(t, batch[2].Error)
	require.Equal(t, new(big.Int).Mul(big.NewInt(100), eth), balance.ToInt())
	require.ErrorContains(t, batch[3].Error, "block not found")
	var re rpc.Error
	require.True(t, errors.As(batch[4].Error, &re))
	require.Equal(t, -32601, re.ErrorCode(), "method not found")
	require.True(t, errors.As(batch[5].Error, &re))
	require.Equal(t, -32602, re.ErrorCode(), "invalid params")
}

func TestRollupSyncStatus(t *testing.T) {
	e := newEnv(t)
	e.l1.QueueDeposit(bob, eth)
	e.rounds(4)
	e.send(e.signed(0, &carol, eth, nil, 21000))
	var st map[string]any
	require.NoError(t, e.seqRPC.Call(&st, "rollup_syncStatus"))
	require.Equal(t, "sequencer", st["role"])
	for _, k := range []string{"unsafe", "safe", "finalized", "l1Head", "l1Cursor", "nextEpoch", "currentEpoch", "l1Reorgs", "unsafeReorgs", "txpool", "derivationLagL1", "batcherNextEpoch", "batchesSubmitted", "batchesAccepted", "fallbackEpochs"} {
		require.Contains(t, st, k)
	}
	require.EqualValues(t, 1, st["txpool"])
	require.GreaterOrEqual(t, st["unsafe"], st["safe"])
	require.EqualValues(t, 0, st["derivationLagL1"])

	var vs map[string]any
	require.NoError(t, e.verRPC.Call(&vs, "rollup_syncStatus"))
	require.Equal(t, "verifier", vs["role"])
	require.Equal(t, st["safe"], vs["safe"])
	require.Equal(t, vs["safe"], vs["unsafe"], "a verifier has no unsafe chain")
	require.NotContains(t, vs, "txpool")
}

func TestVerifierNodeIsReadOnlyAndServesOnlyTheDerivedChain(t *testing.T) {
	e := newEnv(t)
	e.l1.QueueDeposit(bob, new(big.Int).Mul(big.NewInt(3), eth))
	e.rounds(2)
	tx := e.signed(0, &carol, eth, nil, 21000)
	e.send(tx)

	// writes are refused with a pointer to the right node
	err := e.verEth.SendTransaction(bg, e.signed(1, &carol, eth, nil, 21000))
	require.ErrorContains(t, err, "read-only verifier")
	// the transaction is only on the sequencer's unsafe chain so far
	var raw map[string]any
	require.NoError(t, e.verRPC.Call(&raw, "eth_getTransactionByHash", tx.Hash()))
	require.Nil(t, raw, "a verifier does not know pending or unsafe transactions")
	vn, err := e.verEth.PendingNonceAt(bg, user)
	require.NoError(t, err)
	require.EqualValues(t, 0, vn, "pending nonce equals the derived state nonce")

	e.rounds(6)
	sh, err := e.verEth.BlockNumber(bg)
	require.NoError(t, err)
	require.Equal(t, e.der.SafeHead().Number(), sh, "latest on a verifier is the safe head")
	got, err := e.verEth.BalanceAt(bg, carol, nil)
	require.NoError(t, err)
	require.Equal(t, eth.String(), got.String(), "the transfer arrived once it was derived from L1")
	rc, err := e.verEth.TransactionReceipt(bg, tx.Hash())
	require.NoError(t, err)
	require.EqualValues(t, 1, rc.Status)

	// both nodes serve the same block by number and by hash
	sb, err := e.seqEth.HeaderByNumber(bg, new(big.Int).SetUint64(sh))
	require.NoError(t, err)
	vb, err := e.verEth.HeaderByNumber(bg, new(big.Int).SetUint64(sh))
	require.NoError(t, err)
	require.Equal(t, sb.Hash(), vb.Hash())
	var h hexutil.Bytes
	require.NoError(t, e.verRPC.Call(&h, "eth_getCode", carol, "finalized"))
	require.Error(t, e.verRPC.Call(&h, "eth_getCode", carol, "0xfffff"))
}

func TestSafeAndFinalizedTagsOnTheSequencerNode(t *testing.T) {
	e := newEnv(t)
	e.rounds(3)
	e.send(e.signed(0, &carol, eth, nil, 21000))
	e.rounds(3)
	hdr := func(tag string) *gtypes.Header {
		var h *gtypes.Header
		require.NoError(t, e.seqRPC.Call(&h, "eth_getBlockByNumber", tag, false))
		return h
	}
	latest, safe, fin := hdr("latest"), hdr("safe"), hdr("finalized")
	require.GreaterOrEqual(t, latest.Number.Uint64(), safe.Number.Uint64())
	require.GreaterOrEqual(t, safe.Number.Uint64(), fin.Number.Uint64())
	require.Equal(t, e.der.SafeHead().Hash(), safe.Hash())
	e.rounds(14)
	require.Greater(t, hdr("finalized").Number.Uint64(), fin.Number.Uint64(), "finalized advances 12 L1 blocks behind")
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
