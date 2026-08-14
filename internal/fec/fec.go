// Package fec provides a systematic maximum-distance-separable erasure code:
// k data shards are extended to n, and any k of the n reconstruct all k.
//
// It exists because the path this project targets is an erasure channel rather
// than a congested one. About 42% of packets are dropped independently of the
// sending rate (docs/PATH-CHARACTER-20260813.md), and independent loss at that
// rate is cheap to repair and ruinously expensive to retransmit. On a
// memoryless channel with erasure probability p the capacity is (1-p) times the
// line rate whatever the scheme, so coding costs nothing that was ever
// available; retransmitting spends the same budget in round trips instead,
// 1/(1-p) = 1.75 transmissions per packet on average and three or more for 18%
// of them, which at this path's 300 ms is a tail past 600 ms.
//
// The code is systematic, so the data shards are the original bytes and a
// receiver that loses nothing does no work at all. It is MDS, so k arrivals
// always suffice -- never "k of a particular kind".
//
// The generator is [I; C] where C is a Cauchy matrix, C[i][j] = 1/(x_i + y_j)
// over GF(256) with the x and y drawn from disjoint sets. Every square
// submatrix of a Cauchy matrix is invertible, which is exactly the MDS property
// wanted, and it holds by construction rather than by search: an implementation
// that built a Vandermonde matrix and reduced it to systematic form would have
// to check invertibility for each shard count, and could fail for some.
package fec

import (
	"errors"
	"fmt"
)

// MaxShards is the largest n. Each shard needs a distinct field element for
// the Cauchy construction, and GF(256) has 256 of them.
const MaxShards = 256

var (
	// ErrTooFewShards means fewer than k shards survived, so nothing can be
	// reconstructed. It is not a defect: it is the code's rate being wrong for
	// the channel, which is what the rate controller exists to prevent.
	ErrTooFewShards = errors.New("fec: fewer than k shards present")
	ErrShardSize    = errors.New("fec: shards differ in length, or are empty")
	ErrShardCount   = errors.New("fec: wrong number of shards")
)

// Codec encodes and reconstructs one (k, n) block. It holds no per-block state
// and is safe for concurrent use.
type Codec struct {
	k, n int
	// parity is the (n-k) x k Cauchy matrix, row-major.
	parity []byte
}

// New returns a codec for k data shards in n total. It is cheap enough to
// build per block, which matters because the rate controller changes (k, n) as
// the channel moves.
func New(k, n int) (*Codec, error) {
	if k <= 0 || n < k || n > MaxShards {
		return nil, fmt.Errorf("fec: invalid code (%d,%d): need 0 < k <= n <= %d", k, n, MaxShards)
	}
	m := n - k
	c := &Codec{k: k, n: n, parity: make([]byte, m*k)}
	// x and y are disjoint by construction, so x_i + y_j is never zero and
	// every entry is a well-defined inverse.
	for i := 0; i < m; i++ {
		x := byte(k + i)
		for j := 0; j < k; j++ {
			c.parity[i*k+j] = inv(x ^ byte(j))
		}
	}
	return c, nil
}

// DataShards is k and TotalShards is n.
func (c *Codec) DataShards() int  { return c.k }
func (c *Codec) TotalShards() int { return c.n }

// Encode fills in the parity shards. shards must have n entries, all the same
// non-zero length; the first k are read and the rest are overwritten.
func (c *Codec) Encode(shards [][]byte) error {
	size, err := c.checkShards(shards, c.n)
	if err != nil {
		return err
	}
	for i := c.k; i < c.n; i++ {
		clear(shards[i][:size])
		row := c.parity[(i-c.k)*c.k:]
		for j := 0; j < c.k; j++ {
			mulSliceXor(row[j], shards[j][:size], shards[i][:size])
		}
	}
	return nil
}

// Reconstruct rebuilds every missing data shard from whatever arrived. present
// has n entries; a shard whose entry is false is ignored and, if it is a data
// shard, is written. Shards that are present are left untouched.
//
// Parity shards are not rebuilt: a receiver has no use for them once the data
// is whole, and computing them would double the decode cost of the common case
// where only one or two data shards are missing.
func (c *Codec) Reconstruct(shards [][]byte, present []bool) error {
	if len(present) != c.n {
		return ErrShardCount
	}
	size, err := c.shardSize(shards, present)
	if err != nil {
		return err
	}

	// Nothing to do when every data shard arrived, which on a systematic code
	// is the common case and must cost nothing.
	missing := make([]int, 0, c.k)
	for i := 0; i < c.k; i++ {
		if !present[i] {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	// Take the first k shards that arrived. Any k will do -- that is what MDS
	// means -- so there is nothing to choose between them.
	using := make([]int, 0, c.k)
	for i := 0; i < c.n && len(using) < c.k; i++ {
		if present[i] {
			using = append(using, i)
		}
	}
	if len(using) < c.k {
		return ErrTooFewShards
	}

	// The rows of the generator matrix belonging to those shards, inverted:
	// the result maps what arrived back onto the original data.
	sub := make([]byte, c.k*c.k)
	for r, shard := range using {
		copy(sub[r*c.k:(r+1)*c.k], c.generatorRow(shard))
	}
	if err := invert(sub, c.k); err != nil {
		return err
	}

	for _, want := range missing {
		out := shards[want][:size]
		clear(out)
		row := sub[want*c.k : (want+1)*c.k]
		for r, shard := range using {
			mulSliceXor(row[r], shards[shard][:size], out)
		}
		present[want] = true
	}
	return nil
}

// generatorRow returns row i of [I; C] as a k-element slice. The identity rows
// are built on demand rather than stored, which keeps the matrix at (n-k)*k
// bytes instead of n*k.
func (c *Codec) generatorRow(i int) []byte {
	if i >= c.k {
		return c.parity[(i-c.k)*c.k : (i-c.k+1)*c.k]
	}
	row := make([]byte, c.k)
	row[i] = 1
	return row
}

func (c *Codec) checkShards(shards [][]byte, want int) (int, error) {
	if len(shards) != want {
		return 0, ErrShardCount
	}
	size := len(shards[0])
	if size == 0 {
		return 0, ErrShardSize
	}
	for _, s := range shards {
		if len(s) != size {
			return 0, ErrShardSize
		}
	}
	return size, nil
}

// shardSize is checkShards for reconstruction, where absent shards may be
// short or nil and are grown to fit. A caller that hands back a buffer of the
// wrong size for a shard it claims arrived has a bug that must not be papered
// over, so those are still checked exactly.
func (c *Codec) shardSize(shards [][]byte, present []bool) (int, error) {
	if len(shards) != c.n {
		return 0, ErrShardCount
	}
	size := 0
	for i, s := range shards {
		if present[i] && len(s) > size {
			size = len(s)
		}
	}
	if size == 0 {
		return 0, ErrShardSize
	}
	for i, s := range shards {
		if present[i] && len(s) != size {
			return 0, ErrShardSize
		}
		if !present[i] && len(s) < size {
			shards[i] = make([]byte, size)
		}
	}
	return size, nil
}

// invert replaces the k x k matrix with its inverse, by Gauss-Jordan
// elimination against an identity that is carried alongside.
//
// A singular matrix here would mean the Cauchy property failed, which cannot
// happen for a submatrix of [I; C]; it is reported rather than asserted
// because a data path should not panic on a corrupt peer's shard indices.
func invert(matrix []byte, k int) error {
	augmented := make([]byte, k*2*k)
	for r := 0; r < k; r++ {
		copy(augmented[r*2*k:], matrix[r*k:(r+1)*k])
		augmented[r*2*k+k+r] = 1
	}
	for col := 0; col < k; col++ {
		pivot := -1
		for r := col; r < k; r++ {
			if augmented[r*2*k+col] != 0 {
				pivot = r
				break
			}
		}
		if pivot < 0 {
			return errors.New("fec: singular matrix")
		}
		if pivot != col {
			a := augmented[col*2*k : (col+1)*2*k]
			b := augmented[pivot*2*k : (pivot+1)*2*k]
			for i := range a {
				a[i], b[i] = b[i], a[i]
			}
		}
		row := augmented[col*2*k : (col+1)*2*k]
		if lead := row[col]; lead != 1 {
			for i := range row {
				row[i] = div(row[i], lead)
			}
		}
		for r := 0; r < k; r++ {
			if r == col {
				continue
			}
			factor := augmented[r*2*k+col]
			if factor == 0 {
				continue
			}
			target := augmented[r*2*k : (r+1)*2*k]
			for i := range target {
				target[i] ^= mul(factor, row[i])
			}
		}
	}
	for r := 0; r < k; r++ {
		copy(matrix[r*k:(r+1)*k], augmented[r*2*k+k:r*2*k+2*k])
	}
	return nil
}
