// Package fiat defines the monitor's explicitly enabled fiat currencies and
// the DEXes that can be queried for each of them.
package fiat

import "strings"

// Enabled is the stable display and snapshot order. EUR remains first so it
// remains the default wherever a client omits the currency.
var Enabled = []string{"EUR", "USD", "GBP", "AUD", "CHF", "SEK", "NOK", "CAD"}

var enabledSet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(Enabled))
	for _, currency := range Enabled {
		out[currency] = struct{}{}
	}
	return out
}()

// Normalize returns an enabled ISO 4217 currency code, defaulting to EUR.
// The boolean reports whether a non-empty input was recognised.
func Normalize(raw string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if code == "" {
		return "EUR", true
	}
	_, ok := enabledSet[code]
	return code, ok
}

func IsEnabled(raw string) bool {
	_, ok := enabledSet[strings.ToUpper(strings.TrimSpace(raw))]
	return ok
}

// SupportsDEX describes the monitor adapters' supported markets.
func SupportsDEX(dex, currency string) bool {
	if !IsEnabled(currency) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(dex)) {
	case "bisq", "bisq2", "hodlhodl", "mostro":
		return true
	case "peachbitcoin":
		switch strings.ToUpper(currency) {
		case "EUR", "CHF", "GBP", "NOK", "SEK":
			return true
		}
	case "robosats":
		return RoboSatsCurrencyID(currency) != 0
	}
	return false
}

// RoboSatsCurrencyID is the upstream v0 order-book identifier. Keep this
// mapping explicit: the API accepts integers, so a missing code must skip the
// book instead of silently querying the wrong currency.
func RoboSatsCurrencyID(currency string) int {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "USD":
		return 1
	case "EUR":
		return 2
	case "GBP":
		return 4
	case "AUD":
		return 5
	case "CAD":
		return 6
	case "CHF":
		return 7
	case "SEK":
		return 11
	case "NOK":
		return 14
	default:
		return 0
	}
}
