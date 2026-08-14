package fec

import (
	"math"
	"time"

	"github.com/icourses-dev/wanopt/internal/lossmodel"
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
	// InterleaveDepth is the number of blocks the sender spreads its shards
	// across. A burst of consecutive packet losses is divided among that many
	// blocks, so depth is what converts correlated loss back into the
	// independent loss a block code is efficient against. One means no
	// interleaving.
	InterleaveDepth int
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
		// In units of erasure events rather than shards.
		need := int(math.Ceil(float64(k) / burst))
		if need > trials {
			continue
		}
		residual := binomialTailBelow(trials, arrival, need)
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

// effectiveBurst is the mean loss burst as the block sees it, after
// interleaving has divided each burst among the blocks in flight.
func (p Params) effectiveBurst(s lossmodel.Snapshot) float64 {
	burst := s.BurstFactor
	if burst < 1 || math.IsNaN(burst) {
		burst = 1
	}
	if p.InterleaveDepth > 1 {
		burst /= float64(p.InterleaveDepth)
	}
	if burst < 1 {
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
