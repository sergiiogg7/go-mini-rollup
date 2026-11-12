package l1_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	"github.com/sergiiogg7/go-mini-rollup/internal/anviltest"
	"github.com/sergiiogg7/go-mini-rollup/internal/l1"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

var (
	inbox = common.HexToAddress("0x00000000000000000000000000000000000ba7c4")
	alice = common.HexToAddress("0x000000000000000000000000000000000000a11c")
	ether = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

func dial(t *testing.T, n *anviltest.Node, portal common.Address) *l1.Client {
	t.Helper()
	c, err := l1.Dial(n.URL, inbox, portal)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func mustReceipt(t *testing.T, c *l1.Client, h common.Hash) *proto.TxInclusion {
	t.Helper()
	r, err := c.Receipt(h)
	require.NoError(t, err)
	return r
}

// signedWithNonce signs an inbox transaction of the batcher account with a given nonce.
func signedWithNonce(t *testing.T, nonce uint64) []byte {
	t.Helper()
	tx, err := gtypes.SignNewTx(anviltest.BatcherKey, gtypes.LatestSignerForChainID(big.NewInt(anviltest.ChainID)),
		&gtypes.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(5_000_000_000), Gas: 100_000, To: &inbox, Data: []byte("again")})
	require.NoError(t, err)
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	return raw
}

func TestHeadChainIDAndMissingBlocks(t *testing.T) {
	n := anviltest.Start(t)
	c := dial(t, n, common.Address{})
	id, err := c.ChainID()
	require.NoError(t, err)
	require.EqualValues(t, anviltest.ChainID, id)
	head, err := c.Head()
	require.NoError(t, err)
	require.EqualValues(t, 0, head)
	b, err := c.Block(head + 50)
	require.NoError(t, err)
	require.Nil(t, b, "a block that does not exist yet is nil, nil (the derivation waits)")
	genesis, err := c.Block(0)
	require.NoError(t, err)
	require.Empty(t, genesis.Batches)
	require.Empty(t, genesis.Deposits)
}

func TestInboxTransactionsAreReadWithTheirAuthenticSender(t *testing.T) {
	n := anviltest.Start(t)
	c := dial(t, n, common.Address{})
	payload := []byte("not a real batch: the adapter does not decode it")
	tx := n.SendTx(anviltest.BatcherKey, &inbox, nil, payload)
	other := common.HexToAddress("0x000000000000000000000000000000000000dead")
	n.SendTx(anviltest.DeployerKey, &other, big.NewInt(1), []byte("sent to somebody else"))
	n.SendTx(anviltest.DeployerKey, &inbox, nil, []byte("junk from a stranger"))

	var found []proto.BatchTx
	for blk := uint64(1); blk <= n.Head(); blk++ {
		d, err := c.Block(blk)
		require.NoError(t, err)
		require.Equal(t, blk, d.Ref.Number)
		found = append(found, d.Batches...)
	}
	require.Len(t, found, 2, "only transactions to the inbox are batches; the sender decides whether the derivation trusts them")
	require.Equal(t, proto.BatchTx{TxHash: tx.Hash(), From: anviltest.Batcher, Data: payload}, found[0])
	require.Equal(t, anviltest.Deployer, found[1].From)

	d1, _ := c.Block(1)
	d2, _ := c.Block(2)
	require.Equal(t, d1.Ref.Hash, d2.Ref.ParentHash, "refs are chained by hash")
	require.NotZero(t, d1.Ref.Time)
}

func TestDepositLogsAreReadFromTheConfiguredPortalOnly(t *testing.T) {
	n := anviltest.Start(t)
	portal := n.DeployPortal()
	decoy := n.DeployPortal() // same event, other address: must be ignored
	c := dial(t, n, portal)

	n.SendTx(anviltest.DeployerKey, &portal, new(big.Int).Mul(big.NewInt(2), ether), anviltest.DepositSelector(alice))
	n.SendTx(anviltest.DeployerKey, &decoy, ether, anviltest.DepositSelector(alice))
	n.SendTx(anviltest.BatcherKey, &portal, big.NewInt(7), nil) // plain transfer: receive() deposits for the sender

	var deps []proto.Deposit
	for blk := uint64(1); blk <= n.Head(); blk++ {
		d, err := c.Block(blk)
		require.NoError(t, err)
		deps = append(deps, d.Deposits...)
	}
	require.Len(t, deps, 2)
	require.EqualValues(t, 0, deps[0].ID)
	require.Equal(t, alice, deps[0].To)
	require.Equal(t, new(big.Int).Mul(big.NewInt(2), ether), deps[0].Value)
	require.EqualValues(t, 1, deps[1].ID)
	require.Equal(t, anviltest.Batcher, deps[1].To)
	require.EqualValues(t, 7, deps[1].Value.Int64())
}

func TestSubmitterSideNonceGasReceiptAndPendingTransactions(t *testing.T) {
	n := anviltest.Start(t)
	c := dial(t, n, common.Address{})

	price, err := c.GasPrice()
	require.NoError(t, err)
	require.Positive(t, price.Sign())

	nonce, err := c.Nonce(anviltest.Batcher)
	require.NoError(t, err)
	require.EqualValues(t, 0, nonce)

	// a transaction that is not mined yet: pending nonce moves, confirmed nonce and receipt do not
	n.SetAutomine(false)
	tx := n.SendTx(anviltest.BatcherKey, &inbox, nil, []byte("pending"))
	pending, err := c.PendingNonce(anviltest.Batcher)
	require.NoError(t, err)
	require.EqualValues(t, 1, pending)
	confirmed, err := c.Nonce(anviltest.Batcher)
	require.NoError(t, err)
	require.EqualValues(t, 0, confirmed)
	require.Nil(t, mustReceipt(t, c, tx.Hash()), "not in a block yet: nil, nil")

	n.Call(nil, "evm_mine")
	r := mustReceipt(t, c, tx.Hash())
	require.NotNil(t, r)
	require.Equal(t, n.Head(), r.BlockNumber)
	confirmed, _ = c.Nonce(anviltest.Batcher)
	require.EqualValues(t, 1, confirmed)
}

func TestSendRejectsGarbageAndReplayedNonces(t *testing.T) {
	n := anviltest.Start(t)
	c := dial(t, n, common.Address{})
	_, err := c.Send([]byte{0x01, 0x02, 0x03})
	require.Error(t, err, "undecodable bytes never reach the node")

	n.SendTx(anviltest.BatcherKey, &inbox, nil, []byte("one")) // consumes nonce 0
	// a second transaction signed with nonce 0 again: the error must mention the nonce, because the
	// batcher recognises its rebase condition from the error text (batcher.nonceError)
	_, err = c.Send(signedWithNonce(t, 0))
	require.ErrorContains(t, err, "nonce")
}

func TestL1ReorgChangesTheBlockHashAndDropsInboxTransactionsAndDeposits(t *testing.T) {
	n := anviltest.Start(t)
	portal := n.DeployPortal()
	c := dial(t, n, portal)

	dep := n.SendTx(anviltest.DeployerKey, &portal, ether, anviltest.DepositSelector(alice))
	bat := n.SendTx(anviltest.BatcherKey, &inbox, nil, []byte("batch"))
	head := n.Head()
	before, err := c.Block(head)
	require.NoError(t, err)
	depBlock := mustReceipt(t, c, dep.Hash())
	require.NotNil(t, depBlock)
	require.NotNil(t, mustReceipt(t, c, bat.Hash()))

	n.Call(nil, "anvil_reorg", 2, []any{}) // replace the last two blocks with empty ones

	require.Equal(t, head, n.Head(), "same height, different blocks")
	after, err := c.Block(head)
	require.NoError(t, err)
	require.NotEqual(t, before.Ref.Hash, after.Ref.Hash)
	require.Empty(t, after.Batches)
	require.Nil(t, mustReceipt(t, c, bat.Hash()), "the batch transaction left the canonical chain")
	require.Nil(t, mustReceipt(t, c, dep.Hash()), "so did the deposit")
	for blk := depBlock.BlockNumber; blk <= head; blk++ {
		d, err := c.Block(blk)
		require.NoError(t, err)
		require.Empty(t, d.Deposits, "logs are read by block hash: the orphaned deposit cannot leak into the new chain")
	}
}

func TestUnreachableNodeReturnsErrorsInsteadOfPanicking(t *testing.T) {
	c, err := l1.Dial("http://127.0.0.1:1", inbox, common.Address{})
	require.NoError(t, err, "ethclient dials lazily")
	defer c.Close()
	_, err = c.Head()
	require.Error(t, err)
	_, err = c.Block(1)
	require.Error(t, err)
	_, err = c.ChainID()
	require.Error(t, err)
}
