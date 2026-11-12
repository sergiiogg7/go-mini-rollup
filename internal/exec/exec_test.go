package exec

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

const chainID = 901

var (
	keyA, _ = crypto.HexToECDSA("59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d")
	addrA   = crypto.PubkeyToAddress(keyA.PublicKey)
	addrB   = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	eth     = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	// init code that deploys a runtime which returns the 32-byte word 42
	deployCode = common.FromHex("0x600a600c600039600a6000f3602a60005260206000f3")
)

func newEnv(t *testing.T) (*Executor, *proto.Block) {
	t.Helper()
	ex := New(chainID, 30_000_000, common.HexToAddress("0xFEE"))
	cfg := &proto.Config{L1Start: 10, Alloc: map[common.Address]*big.Int{addrA: new(big.Int).Mul(big.NewInt(10), eth)}}
	g, err := ex.Genesis(cfg)
	require.NoError(t, err)
	return ex, g
}

func signLegacy(t *testing.T, nonce uint64, to *common.Address, value *big.Int, data []byte, gas uint64) *gtypes.Transaction {
	t.Helper()
	tx := gtypes.NewTx(&gtypes.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1_000_000_000), Gas: gas, To: to, Value: value, Data: data})
	s, err := gtypes.SignTx(tx, gtypes.NewEIP155Signer(big.NewInt(chainID)), keyA)
	require.NoError(t, err)
	return s
}

func TestTransferDepositAndFees(t *testing.T) {
	ex, g := newEnv(t)
	dep := proto.Deposit{ID: 1, To: addrB, Value: new(big.Int).Set(eth)}
	tx := signLegacy(t, 0, &addrB, eth, nil, 21000)
	r, err := ex.Execute(g.Header.Root, Input{Number: 1, Time: 5, ParentHash: g.Hash(), OriginNumber: 10, Deposits: []proto.Deposit{dep}, Txs: []*gtypes.Transaction{tx}})
	require.NoError(t, err)
	require.Empty(t, r.Skipped)
	require.Len(t, r.Block.Receipts, 1)
	require.Equal(t, gtypes.ReceiptStatusSuccessful, r.Block.Receipts[0].Status)
	st, err := ex.StateAt(r.Block.Header.Root)
	require.NoError(t, err)
	require.Equal(t, "2000000000000000000", st.GetBalance(addrB).ToBig().String()) // deposit + transfer
	fee := new(big.Int).Mul(big.NewInt(21000), big.NewInt(1_000_000_000))
	require.Equal(t, fee.String(), st.GetBalance(common.HexToAddress("0xFEE")).ToBig().String())
	require.Equal(t, uint64(1), st.GetNonce(addrA))
}

func TestContractDeployAndCall(t *testing.T) {
	ex, g := newEnv(t)
	tx := signLegacy(t, 0, nil, new(big.Int), deployCode, 200_000)
	r, err := ex.Execute(g.Header.Root, Input{Number: 1, Time: 5, ParentHash: g.Hash(), OriginNumber: 10, Txs: []*gtypes.Transaction{tx}})
	require.NoError(t, err)
	require.Len(t, r.Block.Receipts, 1)
	addr := r.Block.Receipts[0].ContractAddress
	require.Equal(t, crypto.CreateAddress(addrA, 0), addr)
	res, err := ex.Call(r.Block.Header.Root, 2, 6, CallMsg{From: addrB, To: &addr})
	require.NoError(t, err)
	require.NoError(t, res.Err)
	require.Equal(t, common.LeftPadBytes([]byte{42}, 32), res.ReturnData)
}

func TestInvalidTransactionsAreSkippedNotFatal(t *testing.T) {
	ex, g := newEnv(t)
	good := signLegacy(t, 0, &addrB, big.NewInt(1), nil, 21000)
	badNonce := signLegacy(t, 7, &addrB, big.NewInt(1), nil, 21000)
	tooRich := signLegacy(t, 1, &addrB, new(big.Int).Mul(big.NewInt(100), eth), nil, 21000)
	unprotected, err := gtypes.SignTx(gtypes.NewTx(&gtypes.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1), Gas: 21000, To: &addrB, Value: big.NewInt(1)}), gtypes.HomesteadSigner{}, keyA)
	require.NoError(t, err)
	r, err := ex.Execute(g.Header.Root, Input{Number: 1, Time: 5, ParentHash: g.Hash(), OriginNumber: 10, Txs: []*gtypes.Transaction{good, badNonce, tooRich, unprotected}})
	require.NoError(t, err)
	require.Len(t, r.Block.Txs, 1)
	require.Len(t, r.Skipped, 3)
}

func TestExecutionIsDeterministic(t *testing.T) {
	ex, g := newEnv(t)
	tx := signLegacy(t, 0, &addrB, eth, nil, 21000)
	in := Input{Number: 1, Time: 5, ParentHash: g.Hash(), OriginNumber: 10, OriginHash: common.HexToHash("0x1"), Txs: []*gtypes.Transaction{tx}}
	a, err := ex.Execute(g.Header.Root, in)
	require.NoError(t, err)
	b, err := ex.Execute(g.Header.Root, in)
	require.NoError(t, err)
	require.Equal(t, a.Block.Hash(), b.Block.Hash())
	// a different origin changes the block hash even with identical state
	in.OriginHash = common.HexToHash("0x2")
	c, err := ex.Execute(g.Header.Root, in)
	require.NoError(t, err)
	require.NotEqual(t, a.Block.Hash(), c.Block.Hash())
	require.Equal(t, a.Block.Header.Root, c.Block.Header.Root)
}
