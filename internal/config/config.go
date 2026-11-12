// Package config loads the rollup configuration (the L2 genesis) from a JSON file. Every node of
// the same rollup (sequencer and verifiers) must use the same file.
package config

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"

	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

type file struct {
	ChainID     uint64            `json:"chainId"`
	L1Start     uint64            `json:"l1Start"`
	GenesisTime uint64            `json:"genesisTime"`
	Inbox       common.Address    `json:"inbox"`
	Batcher     common.Address    `json:"batcher"`
	Portal      common.Address    `json:"portal"`
	FeeVault    common.Address    `json:"feeVault"`
	Alloc       map[string]string `json:"alloc"`
	ForkV2At    uint64            `json:"forkV2At"` // 0 = the fork is not scheduled
}

// Load reads and validates a configuration file.
func Load(path string) (*proto.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.ChainID == 0 {
		return nil, fmt.Errorf("%s: chainId is required", path)
	}
	c := &proto.Config{
		L1Start: f.L1Start, GenesisTime: f.GenesisTime, Inbox: f.Inbox, Batcher: f.Batcher, Portal: f.Portal,
		FeeVault: f.FeeVault, Alloc: map[common.Address]*big.Int{},
		Forks: []proto.Fork{{Activation: 0, Params: proto.V1(f.ChainID)}},
	}
	for a, v := range f.Alloc {
		n, ok := new(big.Int).SetString(v, 10)
		if !ok {
			return nil, fmt.Errorf("%s: bad alloc value for %s", path, a)
		}
		c.Alloc[common.HexToAddress(a)] = n
	}
	if f.ForkV2At > 0 {
		c.Forks = append(c.Forks, proto.Fork{Activation: f.ForkV2At, Params: proto.V2(f.ChainID)})
	}
	return c, c.Validate()
}
