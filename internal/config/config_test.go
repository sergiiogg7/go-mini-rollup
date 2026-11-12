package config

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rollup.json")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

const valid = `{
  "chainId": 901, "l1Start": 5, "genesisTime": 1700000000,
  "inbox": "0x00000000000000000000000000000000000ba7c4",
  "batcher": "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
  "portal": "0x5FbDB2315678afecb367f032d93F642f64180aa3",
  "feeVault": "0x000000000000000000000000000000000000fee5",
  "alloc": {"0x000000000000000000000000000000000000b0b0": "1000000000000000000"},
  "forkV2At": 0
}`

func TestLoadValidConfigWithoutFork(t *testing.T) {
	c, err := Load(write(t, valid))
	require.NoError(t, err)
	require.EqualValues(t, 5, c.L1Start)
	require.EqualValues(t, 1700000000, c.GenesisTime)
	require.Equal(t, common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8"), c.Batcher)
	require.Equal(t, common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3"), c.Portal)
	require.Len(t, c.Forks, 1, "forkV2At 0 means the fork is not scheduled")
	require.EqualValues(t, 901, c.Forks[0].Params.ChainID)
	require.Equal(t, big.NewInt(1e18), c.Alloc[common.HexToAddress("0xb0b0")])
}

func TestLoadSchedulesTheForkByL1Block(t *testing.T) {
	c, err := Load(write(t, `{"chainId":901,"l1Start":5,"forkV2At":50}`))
	require.NoError(t, err)
	require.Len(t, c.Forks, 2)
	require.EqualValues(t, 50, c.Forks[1].Activation)
	require.Equal(t, 1, int(c.ParamsAt(60).BatchVersion), "V2 batches commit to the origin hash")
	require.Equal(t, 0, int(c.ParamsAt(49).BatchVersion))
}

func TestLoadRejectsBadConfigurations(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"missing chain id", `{"l1Start":1}`, "chainId is required"},
		{"zero l1Start", `{"chainId":901,"l1Start":0}`, "l1Start must be >= 1"},
		{"not json", `{"chainId":`, "parse"},
		{"misspelled key would silently drop the fork", `{"chainId":901,"l1Start":1,"forkV2":10}`, "unknown field"},
		{"wrong type", `{"chainId":"901","l1Start":1}`, "parse"},
		{"bad address", `{"chainId":901,"l1Start":1,"inbox":"not-an-address"}`, "parse"},
		{"bad alloc amount", `{"chainId":901,"l1Start":1,"alloc":{"0x01":"12abc"}}`, "bad alloc value"},
		{"empty alloc amount", `{"chainId":901,"l1Start":1,"alloc":{"0x01":""}}`, "bad alloc value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.content))
			require.ErrorContains(t, err, c.want)
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
