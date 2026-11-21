package routing

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"commons/bitcoin"
)

func assertFloat(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %.12f, want %.12f", got, want)
	}
}

// ---- conversion cost model (ported crossover numbers) ----

func crossoverParams() swapParams {
	p := defaultSwapParams()
	p.FeeRate = 20
	p.ServiceFrac = 0.001 // 0.1%
	p.ClaimMinerSats = 150
	p.RouteFrac = 0
	return p
}

func TestChannelOpenAndSwapPerPaymentCosts(t *testing.T) {
	p := crossoverParams()
	// channel open (+close) = (153 + 140) * 20 = 5860 sats
	assertFloat(t, p.channelOpenCostSats(), 5860)
	// per-swap at 1,000,000 sats = lockup(150*20) + service(0.001*1e6) + miner(150) = 4150
	assertFloat(t, p.swapPerPaymentCostSats(1_000_000), 4150)
}

func TestCrossoverIsInformationalAndStrategyStaysSubmarineSwap(t *testing.T) {
	p := crossoverParams()
	xo, ok := p.crossoverSteadyStateSats()
	if !ok {
		t.Fatal("crossover should be defined when a percentage fee exists")
	}
	// A* = (5860 - 3000 - 150) / 0.001 = 2,710,000 sats
	assertFloat(t, xo, 2_710_000)

	below := p.conversionBreakdown(0.02) // 2,000,000 sats
	if below.Strategy != StrategySubmarineSwap {
		t.Fatalf("below crossover strategy = %q, want submarine_swap", below.Strategy)
	}
	above := p.conversionBreakdown(0.03) // 3,000,000 sats
	if above.Strategy != StrategySubmarineSwap {
		t.Fatalf("above crossover strategy = %q, want submarine_swap", above.Strategy)
	}
	assertFloat(t, above.FeeSumBTC, round8(p.swapPerPaymentCostSats(3_000_000)/1e8))
}

func TestParseBoltzMinerFeesNumberAndObject(t *testing.T) {
	assertFloat(t, parseBoltzMinerFees(json.RawMessage(`148`)), 148)
	assertFloat(t, parseBoltzMinerFees(json.RawMessage(`{"lockup":120,"claim":28}`)), 148)
	assertFloat(t, parseBoltzMinerFees(json.RawMessage(``)), 0)
	assertFloat(t, parseBoltzMinerFees(json.RawMessage(`"oops"`)), 0)
}

// ---- per-DEX, per-side routing matrix ----

func TestMethodForMatrix(t *testing.T) {
	cases := []struct {
		dex  string
		side Side
		want Kind
	}{
		{"bisq", SideBid, MethodOnChainDirect},
		{"bisq", SideAsk, MethodOnChainDirect},
		{"bisq2", SideBid, MethodOnChainHubWallet},
		{"bisq2", SideAsk, MethodOnChainDirect},
		{"hodlhodl", SideBid, MethodOnChainHubWallet},
		{"hodlhodl", SideAsk, MethodOnChainDirect},
		{"robosats", SideBid, MethodOnChainSwap},
		{"robosats", SideAsk, MethodOnChainSwap},
		{"mostro", SideBid, MethodOnChainSwap},
		{"mostro", SideAsk, MethodOnChainSwap},
		{"peachbitcoin", SideBid, MethodOnChainHubWallet},
		{"peachbitcoin", SideAsk, MethodOnChainDirect},
		// Unknown DEX falls back to the safe default (hub on bid, direct on ask).
		{"someother", SideBid, MethodOnChainHubWallet},
		{"someother", SideAsk, MethodOnChainDirect},
	}
	for _, c := range cases {
		if got := MethodFor(c.dex, c.side); got != c.want {
			t.Fatalf("MethodFor(%q, %q) = %q, want %q", c.dex, c.side, got, c.want)
		}
		// There is no Lightning-direct method anywhere.
		if got := MethodFor(c.dex, c.side); got != MethodOnChainDirect && got != MethodOnChainHubWallet && got != MethodOnChainSwap {
			t.Fatalf("unexpected method %q", got)
		}
	}
}

func TestSideForDirection(t *testing.T) {
	if SideForDirection("ask") != SideAsk {
		t.Fatal("sell should map to ask")
	}
	for _, d := range []string{"bid", "", "anything"} {
		if SideForDirection(d) != SideBid {
			t.Fatalf("%q should map to bid", d)
		}
	}
}

// ---- engine hop counts via a fake on-chain fee source ----

type fakeFeeSrc struct {
	perTxBTC float64
	satVB    int64
}

func (fakeFeeSrc) IsDataStale(time.Duration) bool { return false }
func (fakeFeeSrc) FetchLatestData() error         { return nil }
func (f fakeFeeSrc) GetDetailedRecommendationForTier(in, out int, txType string, confs int, tier string) (*bitcoin.DetailedFeeContext, error) {
	return &bitcoin.DetailedFeeContext{
		Recommendation: &bitcoin.FeeRecommendation{TotalFeeBTC: f.perTxBTC, FeeRateSatVB: f.satVB, Tier: "standard"},
		Estimate:       &bitcoin.TransactionCostEstimate{EstimatedConfidence: 0.9},
		TxType:         txType,
		TxInputs:       in,
		TxOutputs:      out,
	}, nil
}
func (fakeFeeSrc) EstimateLightningRoutingFee(amountBTC float64) float64 { return 0 }

func newTestEngine(perTxBTC float64, satVB int64) *Engine {
	e := newEngineWithSource(fakeFeeSrc{perTxBTC: perTxBTC, satVB: satVB})
	// Seed the Boltz cache so the swap method never touches the network in tests.
	e.boltz.value = boltzFees{serviceFrac: 0.001, minerSats: 150}
	e.boltz.fetchedAt = time.Now()
	return e
}

func TestEngineHopCounts(t *testing.T) {
	e := newTestEngine(0.00005, 10)
	in := Input{BTCAmount: 0.01, RequiredConfirmations: 1, WithdrawalTx: TxShape{TxType: "P2WPKH", Inputs: 1, Outputs: 2}}

	direct := e.Cost(MethodOnChainDirect, in)
	if direct.NumOnchainTxs != 1 {
		t.Fatalf("direct txs = %d, want 1", direct.NumOnchainTxs)
	}
	assertFloat(t, direct.NetworkFeeBTC, 0.00005)
	if direct.Conversion.Strategy != "" {
		t.Fatalf("direct should have no conversion, got %q", direct.Conversion.Strategy)
	}

	hub := e.Cost(MethodOnChainHubWallet, in)
	if hub.NumOnchainTxs != 2 {
		t.Fatalf("hub txs = %d, want 2", hub.NumOnchainTxs)
	}
	assertFloat(t, hub.NetworkFeeBTC, 0.0001) // 2 × per-tx

	swap := e.Cost(MethodOnChainSwap, in)
	if swap.NumOnchainTxs != 1 {
		t.Fatalf("swap txs = %d, want 1", swap.NumOnchainTxs)
	}
	assertFloat(t, swap.NetworkFeeBTC, 0.00005)
	if swap.Conversion.Strategy != StrategySubmarineSwap {
		t.Fatalf("swap conversion strategy = %q, want submarine_swap", swap.Conversion.Strategy)
	}
	// service 0.1% of 0.01 BTC (1000 sats) + miner 150 + lockup 150vB×10 (1500 sats) = 2650 sats
	assertFloat(t, swap.Conversion.FeeSumBTC, 0.0000265)
}
