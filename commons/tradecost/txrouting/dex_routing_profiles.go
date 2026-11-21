package routing

import "sort"

// DexRoutingProfile maps the two sides of a DEX offer to the routing method that is
// physically valid for that side. There is no cost comparison between methods — the
// DEX's wallet mechanics determine the route.
type DexRoutingProfile struct {
	Bid Kind // we sell BTC on the DEX (BTC leaves toward the counterparty/escrow)
	Ask Kind // we buy BTC on the DEX (BTC arrives at our own CEX deposit)
}

// dexTxRoutingProfiles is the per-DEX routing matrix. See the deciding rule:
//   - Direct: destination is an address we control / can whitelist (our Bisq wallet,
//     or — on the ask side, for every on-chain DEX — our own CEX deposit address).
//   - HubWallet: destination is a third-party address that changes per trade and
//     cannot be pre-whitelisted at the CEX, so we forward through our LND hub.
//   - Swap: the BTC leg is Lightning (RoboSats), reachable only via a local swap.
var dexTxRoutingProfiles = map[string]DexRoutingProfile{
	// Bisq v1: integrated BTC wallet we control; withdraw straight into it (it funds
	// the per-trade multisig itself). Ask payout goes to an address we specify.
	"bisq": {Bid: MethodOnChainDirect, Ask: MethodOnChainDirect},
	// Bisq Easy: reputation-based direct seller→buyer on-chain send to the buyer's
	// per-trade address → hub on bid; ask receives at our CEX deposit.
	"bisq2": {Bid: MethodOnChainHubWallet, Ask: MethodOnChainDirect},
	// HodlHodl: non-custodial 2-of-3 multisig, per-trade P2WSH escrow → hub on bid.
	"hodlhodl": {Bid: MethodOnChainHubWallet, Ask: MethodOnChainDirect},
	// RoboSats: Lightning BTC leg (hold invoices) → swap on both sides.
	"robosats": {Bid: MethodOnChainSwap, Ask: MethodOnChainSwap},
	// Mostro: Lightning hold-invoice escrow, same swap route as RoboSats.
	"mostro": {Bid: MethodOnChainSwap, Ask: MethodOnChainSwap},
	// Peach: on-chain P2P with a per-trade escrow address (like HodlHodl) → hub on bid.
	"peachbitcoin": {Bid: MethodOnChainHubWallet, Ask: MethodOnChainDirect},
}

// defaultDexRoutingProfile is used for DEXes not in the matrix: hub on bid (safe for
// an unknown changing destination), direct on ask (always our own CEX deposit).
var defaultDexRoutingProfile = DexRoutingProfile{Bid: MethodOnChainHubWallet, Ask: MethodOnChainDirect}

// ProfileFor returns the routing profile for a DEX, falling back to a safe default.
func ProfileFor(dexExchange string) DexRoutingProfile {
	if p, ok := dexTxRoutingProfiles[dexExchange]; ok {
		return p
	}
	return defaultDexRoutingProfile
}

// MethodFor returns the routing method for a DEX and offer side.
func MethodFor(dexExchange string, side Side) Kind {
	p := ProfileFor(dexExchange)
	if side == SideAsk {
		return p.Ask
	}
	return p.Bid
}

// SupportedDEXes returns the DEX names that have an explicit routing profile,
// sorted for stable output.
func SupportedDEXes() []string {
	names := make([]string, 0, len(dexTxRoutingProfiles))
	for name := range dexTxRoutingProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
