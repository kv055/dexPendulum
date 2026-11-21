package accounts

import "strings"

// CategoryForProvider returns the canonical account category for a known provider
// name. Returns "" for unknown providers (e.g. bank institutions).
func CategoryForProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "wise", "paypal", "revolut", "n26":
		return "Fiat Transmitter"
	case "robosats", "bisq", "bisq2", "hodlhodl", "peach", "peach bitcoin", "peachbitcoin", "mostro":
		return "DEX"
	case "kraken", "coinbase", "bitstamp", "binance", "bybit", "kucoin", "huobi":
		return "CEX"
	default:
		return ""
	}
}
