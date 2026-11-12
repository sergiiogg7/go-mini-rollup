// Package anviltest starts a throwaway Anvil (Foundry's local L1) for integration tests. Tests skip
// when `anvil` is not installed, unless REQUIRE_ANVIL=1 (set in CI) turns that into a failure so a
// missing toolchain can never hide the tests.
package anviltest

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Anvil's public development accounts 0 and 1 (never use them anywhere real).
var (
	DeployerKey, _ = crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	BatcherKey, _  = crypto.HexToECDSA("59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d")
	Deployer       = crypto.PubkeyToAddress(DeployerKey.PublicKey)
	Batcher        = crypto.PubkeyToAddress(BatcherKey.PublicKey)
)

// ChainID is Anvil's default chain id.
const ChainID = 31337

// Node is a running Anvil.
type Node struct {
	URL    string
	RPC    *rpc.Client
	Eth    *ethclient.Client
	t      *testing.T
	manual bool // automine switched off: SendTx must not wait for a block
}

// Start launches Anvil with automine (one block per transaction) and stops it with the test.
func Start(t *testing.T) *Node {
	t.Helper()
	bin, err := exec.LookPath("anvil")
	if err != nil {
		if os.Getenv("REQUIRE_ANVIL") == "1" {
			t.Fatal("REQUIRE_ANVIL=1 but anvil is not installed")
		}
		t.Skip("anvil not installed: skipping the integration test")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "--port", fmt.Sprint(port), "--silent")
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	n := &Node{URL: fmt.Sprintf("http://127.0.0.1:%d", port), t: t}
	for i := 0; i < 100; i++ {
		c, err := rpc.Dial(n.URL)
		if err == nil {
			var id string
			if c.Call(&id, "eth_chainId") == nil {
				n.RPC, n.Eth = c, ethclient.NewClient(c)
				t.Cleanup(c.Close)
				return n
			}
			c.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("anvil did not start")
	return nil
}

// SetAutomine turns one-block-per-transaction on or off; with it off, mine with Call(nil, "evm_mine").
func (n *Node) SetAutomine(on bool) {
	n.t.Helper()
	n.Call(nil, "evm_setAutomine", on)
	n.manual = !on
}

// Call invokes a JSON-RPC method (for anvil_* and evm_* helpers).
func (n *Node) Call(result any, method string, args ...any) {
	n.t.Helper()
	if err := n.RPC.Call(result, method, args...); err != nil {
		n.t.Fatalf("%s: %v", method, err)
	}
}

// Head is the latest block number.
func (n *Node) Head() uint64 {
	n.t.Helper()
	h, err := n.Eth.BlockNumber(context.Background())
	if err != nil {
		n.t.Fatal(err)
	}
	return h
}

// SendTx signs and sends a legacy transaction. With automine it is mined immediately.
func (n *Node) SendTx(key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) *types.Transaction {
	n.t.Helper()
	ctx := context.Background()
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, err := n.Eth.PendingNonceAt(ctx, from)
	if err != nil {
		n.t.Fatal(err)
	}
	price, err := n.Eth.SuggestGasPrice(ctx)
	if err != nil {
		n.t.Fatal(err)
	}
	if value == nil {
		value = new(big.Int)
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(ChainID)), &types.LegacyTx{
		Nonce: nonce, GasPrice: new(big.Int).Mul(price, big.NewInt(2)), Gas: 2_000_000, To: to, Value: value, Data: data,
	})
	if err != nil {
		n.t.Fatal(err)
	}
	if err := n.Eth.SendTransaction(ctx, tx); err != nil {
		n.t.Fatal(err)
	}
	if !n.manual { // the node answers before the block is visible to every query: wait for it
		if _, err := bind.WaitMined(ctx, n.Eth, tx); err != nil {
			n.t.Fatal(err)
		}
	}
	return tx
}

// DeployPortal builds contracts/src/DepositPortal.sol with forge and deploys it from the deployer
// account, so the tests read the logs of the real contract, not of a hand-written stand-in.
func (n *Node) DeployPortal() common.Address {
	n.t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "contracts")
	build := exec.Command("forge", "build", "--root", root, "--skip", "test", "--skip", "script")
	if out, err := build.CombinedOutput(); err != nil {
		if os.Getenv("REQUIRE_ANVIL") != "1" {
			n.t.Skipf("forge build failed (%v): %s", err, out)
		}
		n.t.Fatalf("forge build: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(root, "out", "DepositPortal.sol", "DepositPortal.json"))
	if err != nil {
		n.t.Fatal(err)
	}
	var art struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &art); err != nil {
		n.t.Fatal(err)
	}
	tx := n.SendTx(DeployerKey, nil, nil, common.FromHex(art.Bytecode.Object))
	rc, err := bind.WaitMined(context.Background(), n.Eth, tx)
	if err != nil || rc.Status != 1 {
		n.t.Fatalf("portal deployment failed: %v %+v", err, rc)
	}
	return rc.ContractAddress
}

// DepositSelector is the 4-byte selector of deposit(address).
func DepositSelector(to common.Address) []byte {
	return append(crypto.Keccak256([]byte("deposit(address)"))[:4], common.LeftPadBytes(to.Bytes(), 32)...)
}
