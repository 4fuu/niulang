package fec

import (
	"math"
	"time"

	"github.com/bojieli/queqiao/internal/lossmodel"
)

// Class is what a flow needs from the code. It decides the block length, which
// is the only place latency and efficiency genuinely trade against each other.
type Class int

const (
	// ClassBulk wants the code rate as close to capacity as the channel
	// allows, and can wait a block to get it.
	ClassBulk Class = iota
	// ClassInteractive wants a repair sooner than a long block can deliver
	// one, and pays for it in parity.
	ClassInteractive
)

// Params describes the flow the code is for. Everything here except the class
// is measured, not configured.
type Params struct {
	Class Class
	// ShardBytes is the payload one shard carries, normally the path MTU less
	// headers.
	ShardBytes int
	// RateBytesPerSec is the flow's current sending rate. With the block
	// length fixed, this is what converts it into a delay.
	RateBytesPerSec float64
	// RoundTrip is the path's minimum round trip. It bounds the useful block
	// length: a block that takes longer to send than a retransmission takes to
	// arrive has given up the only thing coding was for.
	RoundTrip time.Duration
	// TargetResidual is the acceptable probability that a block arrives
	// unrepairable and has to be retransmitted. It should not be zero: driving
	// it down costs parity geometrically, and the residual is exactly what
	// retransmission is good at.
	TargetResidual float64
}

// Plan is a code chosen for a channel.
type Plan struct {
	// Code is false when parity is not worth sending, and everything below is
	// meaningless.
	Code bool
	K, N int
	// Rate is K/N, the fraction of the wire that carries data.
	Rate float64
	// Residual is the estimated probability that a block cannot be repaired
	// and must be retransmitted.
	Residual float64
	// LossCoded is the erasure probability the code was sized for.
	LossCoded float64
	// EffectiveBurst is the mean loss burst after interleaving, in shards of
	// one block. It is what made the code weaker than the loss rate alone
	// would suggest.
	EffectiveBurst float64
	// Why records the reason in one phrase, for logs and traces.
	Why string
}

// MaxShards is the largest block a block code can have. Each shard needs a
// distinct field element for the generator's construction, and GF(256) has 256
// of them.
//
// It bounds ShardsFor, which sizes a genuine block: the burst a producer has
// just finished, with nothing following it to share parity with. The sliding
// window is not bounded by it -- its coefficients are drawn per repair over at
// most a window's symbols, so it can send as many repairs as the channel needs
// (see WindowRate).
const MaxShards = 256

const (
	// minCodedLoss is the erasure rate below which parity costs more than it
	// saves. Below it a retransmission is rare enough to be cheap, and every
	// parity shard is bandwidth taken from data on a path where bandwidth is
	// the scarce thing.
	minCodedLoss = 0.005
	// minShards keeps a block long enough that the binomial has some shape. A
	// two-shard block at 42% loss needs a rate near a third to be reliable,
	// which is worse than not coding.
	minShards = 8
	// minRate stops the search descending into codes that spend more than
	// eight times the wire per byte delivered. Past that the channel is not
	// one this transport can use, and saying so is more useful than pretending.
	minRate = 0.125
)

// Choose sizes a code for what the estimator currently believes about the path.
//
// It sizes for the loss floor rather than the loss rate. The floor is the part
// of the loss that does not respond to sending more slowly, so it is the part
// that has to be coded around; the excess above it is congestion, and parity
// added to an overflowing queue is more of exactly what overflowed it. The two
// are not separated by policy but by the estimator's min filter, which is also
// what makes this self-correcting: congestion that persists long enough to
// leave the filter's window becomes the floor, and then it does get coded for.
func Choose(s lossmodel.Snapshot, p Params) Plan {
	loss := s.Floor
	if loss <= 0 {
		loss = s.Loss
	}
	if loss < minCodedLoss {
		return Plan{Why: "loss below the parity's cost"}
	}
	if p.ShardBytes <= 0 || p.TargetResidual <= 0 || p.TargetResidual >= 1 {
		return Plan{Why: "no usable parameters"}
	}

	n := p.blockShards()
	burst := p.effectiveBurst(s)
	arrival := 1 - loss

	// A burst that takes several shards of the same block at once is one
	// erasure event, not several independent ones, so the block carries fewer
	// independent trials than it has shards. Sizing the code as if every shard
	// were an independent trial is the error that would certify a rate the
	// path defeats.
	trials := int(math.Round(float64(n) / burst))
	if trials < 1 {
		trials = 1
	}

	// Search down from the highest rate that could work. The tail is monotone
	// in k, so the first k that meets the target is the best one.
	best := Plan{LossCoded: loss, EffectiveBurst: burst}
	for k := n; k >= 1; k-- {
		if float64(k)/float64(n) < minRate {
			break
		}
		residual, ok := residualFor(k, trials, burst, arrival)
		if !ok {
			continue
		}
		if residual <= p.TargetResidual {
			best.Code = true
			best.K, best.N = k, n
			best.Rate = float64(k) / float64(n)
			best.Residual = residual
			best.Why = "sized for the loss floor"
			return best
		}
	}
	best.Why = "no code rate above the floor meets the target residual"
	return best
}

// ShardsFor answers Choose's question in the other direction: given how many
// data shards a block actually holds, the smallest total that meets the target
// residual.
//
// A block is not always the length the plan assumed. A flow that flushes a
// short write seals a block of one or two shards, and sizing that block by the
// plan's rate would be wrong in both directions -- it would send the plan's
// whole block length for a few bytes, and it would still be too weak, because
// a short block has no room for the binomial to average out. Repairing one
// shard at 42% loss needs eight copies to reach a residual of a thousandth,
// and that is the honest answer rather than the rate the long blocks use.
func ShardsFor(k int, s lossmodel.Snapshot, p Params) (int, bool) {
	if k < 1 {
		return 0, false
	}
	loss := s.Floor
	if loss <= 0 {
		loss = s.Loss
	}
	if loss < minCodedLoss || p.TargetResidual <= 0 || p.TargetResidual >= 1 {
		return k, false
	}
	burst := p.effectiveBurst(s)
	arrival := 1 - loss
	for n := k; n <= MaxShards; n++ {
		trials := int(math.Round(float64(n) / burst))
		if trials < 1 {
			trials = 1
		}
		residual, ok := residualFor(k, trials, burst, arrival)
		if !ok {
			continue
		}
		if residual <= p.TargetResidual {
			return n, true
		}
	}
	return 0, false
}

// WindowRate is how many repair symbols each source symbol earns on a sliding
// window of the given capacity, for the target residual.
//
// It is not ShardsFor's answer, and the difference is not small. ShardsFor
// sizes a block, where the only transmissions that can repair an erasure are
// the ones in its own block; on a window they chain, because a repair that
// resolves a neighbouring symbol frees an equation that covers this one, and
// that equation may come from a window this symbol was never in. The code
// therefore behaves like a block several times the window's length, and asking
// for a block's parity over a window's length buys a residual far below the
// one that was asked for -- at 42% erasure and a window of 64, 1.20 repairs
// per symbol where 0.98 was enough, which is a fifth of the wire.
//
// The multiple is measured rather than derived: the chaining depends on how
// the decoder retains equations, which is a property of this implementation
// and not of the arithmetic. TestTheWindowRateIsWhatTheWindowNeeds holds it to
// what the code actually achieves.
//
// The block's shard limit does not apply here either. A block of 256 shards is
// all GF(256) has distinct generator rows for, so ShardsFor gives up above it
// and reports that no code will do -- but a window's coefficients are drawn per
// repair over at most a window's symbols, so the repairs are unbounded and a
// wide window is exactly where the code is cheapest.
func WindowRate(capacity int, s lossmodel.Snapshot, p Params) float64 {
	if capacity < 1 {
		return 0
	}
	loss := s.Floor
	if loss <= 0 {
		loss = s.Loss
	}
	if loss < minCodedLoss || p.TargetResidual <= 0 || p.TargetResidual >= 1 {
		return 0
	}
	arrival := 1 - loss
	if arrival <= 0 {
		return maxWindowRate
	}
	effective := int(float64(capacity) * windowChaining)
	// The tail falls as transmissions are added, so the smallest total that
	// meets the target is a bisection rather than a walk.
	lo, hi := effective, int(float64(effective)/arrival*maxWindowRate)
	if binomialTailBelow(hi, arrival, effective) > p.TargetResidual {
		return maxWindowRate
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if binomialTailBelow(mid, arrival, effective) <= p.TargetResidual {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return float64(lo-effective) / float64(effective)
}

const (
	// windowChaining is how many windows' worth of symbols one window's repairs
	// effectively code over, once equations that resolve neighbouring symbols
	// are counted. Measured, not derived.
	windowChaining = 2.5
	// maxWindowRate stops the search where the channel is no longer one this
	// transport can use, matching minRate's judgement for blocks.
	maxWindowRate = 1 / minRate
)

// residualFor is the probability that a block of k data shards cannot be
// repaired, given how many independent erasure events its total length carries.
// It reports ok=false when the block is too short to hold k shards' worth of
// events at all.
func residualFor(k, trials int, burst, arrival float64) (float64, bool) {
	// In units of erasure events rather than shards.
	need := int(math.Ceil(float64(k) / burst))
	if need > trials {
		return 1, false
	}
	return binomialTailBelow(trials, arrival, need), true
}

// blockShards picks n from the latency the flow can afford. Coding's whole
// latency advantage is that a repair costs a block rather than a round trip, so
// a block is never allowed to take longer than a round trip; an interactive
// flow gets a tighter bound still.
func (p Params) blockShards() int {
	n := MaxShards
	budget := p.RoundTrip
	if p.Class == ClassInteractive {
		// A quarter of the round trip: long enough that the code has shape,
		// short enough that a repaired packet still beats a retransmission by
		// a wide margin.
		budget = p.RoundTrip / 4
	}
	if budget > 0 && p.RateBytesPerSec > 0 {
		fits := int(budget.Seconds() * p.RateBytesPerSec / float64(p.ShardBytes))
		if fits < n {
			n = fits
		}
	}
	if n < minShards {
		n = minShards
	}
	if n > MaxShards {
		n = MaxShards
	}
	return n
}

// effectiveBurst is the mean loss burst as the block sees it.
//
// Correlation is answered by lowering the code rate, not by spreading a
// block's shards across others. Interleaving is the alternative answer, and it
// trades latency for rate: a block is not complete until every block it was
// interleaved with has been sent, which gives back exactly what coding was
// bought for. Nothing measured on this path asks for that trade -- below the
// knee the channel is memoryless, so there is no correlation to undo, and
// above it the right response is to send less rather than to code harder.
func (p Params) effectiveBurst(s lossmodel.Snapshot) float64 {
	burst := s.BurstFactor
	if burst < 1 || math.IsNaN(burst) {
		burst = 1
	}
	return burst
}

// binomialTailBelow returns P(X < k) for X ~ Binomial(n, q), computed in log
// space so a long block's factorials do not overflow.
func binomialTailBelow(n int, q float64, k int) float64 {
	if k <= 0 {
		return 0
	}
	if k > n {
		return 1
	}
	if q <= 0 {
		return 1
	}
	if q >= 1 {
		return 0
	}
	logQ, logP := math.Log(q), math.Log1p(-q)
	logN1, _ := math.Lgamma(float64(n) + 1)
	total := 0.0
	for i := 0; i < k; i++ {
		logI1, _ := math.Lgamma(float64(i) + 1)
		logNI1, _ := math.Lgamma(float64(n-i) + 1)
		total += math.Exp(logN1 - logI1 - logNI1 + float64(i)*logQ + float64(n-i)*logP)
	}
	if total > 1 {
		return 1
	}
	return total
}

// Overhead is the bytes on the wire per byte of data this plan delivers, which
// is what a code rate means to the flow paying for it.
func (p Plan) Overhead() float64 {
	if !p.Code || p.K == 0 {
		return 1
	}
	return float64(p.N) / float64(p.K)
}
