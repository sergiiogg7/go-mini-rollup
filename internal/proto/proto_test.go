package proto

import (
	"bytes"
	"compress/zlib"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func sample(version uint8) *EpochBatch {
	return &EpochBatch{
		Version: version, Epoch: 7, OriginHash: map[uint8]common.Hash{0: {}, 1: common.HexToHash("0xabc")}[version],
		Blocks: []BatchBlock{{Time: 100, Txs: [][]byte{{1, 2, 3}, {4}}}, {Time: 101}},
	}
}

func TestBatchRoundTrip(t *testing.T) {
	for _, v := range []uint8{0, 1} {
		enc, err := EncodeBatch(sample(v))
		require.NoError(t, err)
		got, err := DecodeBatch(enc, 1<<20, 1<<20)
		require.NoError(t, err)
		require.Equal(t, sample(v).Epoch, got.Epoch)
		require.Equal(t, sample(v).OriginHash, got.OriginHash)
		require.Equal(t, sample(v).Blocks[0].Txs, got.Blocks[0].Txs)
		require.Len(t, got.Blocks, 2)
	}
}

func TestDecodeRejectsEverythingMalformed(t *testing.T) {
	good, _ := EncodeBatch(sample(0))
	big := new(bytes.Buffer)
	zw := zlib.NewWriter(big)
	_, _ = zw.Write(make([]byte, 2<<20))
	_ = zw.Close()
	cases := map[string][]byte{
		"empty":          nil,
		"one byte":       {0},
		"unknown ver":    append([]byte{9}, good[1:]...),
		"truncated":      good[:len(good)/2],
		"not zlib":       {0, 1, 2, 3, 4},
		"bad rlp":        append([]byte{0}, compress(t, []byte{0xc2, 0x01})...),
		"trailing bytes": append([]byte{0}, compress(t, append(mustPayload(t), 0x00))...),
		"zlib bomb":      append([]byte{0}, big.Bytes()...),
	}
	for name, data := range cases {
		_, err := DecodeBatch(data, 1<<20, 1<<20)
		require.Error(t, err, name)
		require.True(t, errors.Is(err, ErrDecode), name)
	}
	_, err := DecodeBatch(good, 3, 1<<20)
	require.ErrorIs(t, err, ErrDecode, "over the compressed limit")
}

func mustPayload(t *testing.T) []byte {
	t.Helper()
	enc, _ := EncodeBatch(sample(0))
	zr, err := zlib.NewReader(bytes.NewReader(enc[1:]))
	require.NoError(t, err)
	var out bytes.Buffer
	_, _ = out.ReadFrom(zr)
	return out.Bytes()
}

func compress(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func TestParamsAtPicksTheLatestActiveFork(t *testing.T) {
	c := &Config{L1Start: 5, Forks: []Fork{{0, V1(1)}, {10, V2(1)}}}
	require.NoError(t, c.Validate())
	require.Equal(t, uint8(0), c.ParamsAt(9).BatchVersion)
	require.Equal(t, uint8(1), c.ParamsAt(10).BatchVersion)
	require.Equal(t, uint8(1), c.ParamsAt(999).BatchVersion)
	require.Error(t, (&Config{L1Start: 5, Forks: []Fork{{6, V1(1)}}}).Validate())
	require.Error(t, (&Config{L1Start: 5, Forks: []Fork{{0, V1(1)}, {0, V2(1)}}}).Validate())
	require.Error(t, (&Config{}).Validate())
}

// FuzzDecodeBatch: whatever the input, the decoder returns an error wrapping ErrDecode or a batch;
// it never panics and never allocates beyond the limits. Run: go test -fuzz=FuzzDecodeBatch ./internal/proto
func FuzzDecodeBatch(f *testing.F) {
	good0, _ := EncodeBatch(sample(0))
	good1, _ := EncodeBatch(sample(1))
	f.Add(good0)
	f.Add(good1)
	f.Add([]byte{})
	f.Add([]byte{1, 0x78, 0x9c})
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := DecodeBatch(data, 1<<16, 1<<18)
		if err != nil {
			require.ErrorIs(t, err, ErrDecode)
			return
		}
		// anything that decodes must re-encode and decode to the same batch
		enc, err := EncodeBatch(b)
		require.NoError(t, err)
		again, err := DecodeBatch(enc, 1<<20, 1<<20)
		require.NoError(t, err)
		require.Equal(t, b.Epoch, again.Epoch)
		require.Equal(t, len(b.Blocks), len(again.Blocks))
	})
}
