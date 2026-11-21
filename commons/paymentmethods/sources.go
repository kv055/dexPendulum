package paymentmethods

import (
	"fmt"
	"strings"

	"commons/fiat"
)

// AllowedSources are the DEX offer slugs a payment method may list in
// payment_methods.sources. Keep in sync with offers.exchange CHECK and the
// payment_methods_sources_allowed_check constraint.
var AllowedSources = []string{
	"bisq",
	"bisq2",
	"hodlhodl",
	"robosats",
	"peachbitcoin",
	"mostro",
}

var allowedSourceSet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(AllowedSources))
	for _, source := range AllowedSources {
		out[source] = struct{}{}
	}
	return out
}()

// NormalizeSources lowercases, trims, deduplicates, and validates DEX source
// slugs. Empty input yields an empty (non-nil) slice.
func NormalizeSources(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		source := strings.ToLower(strings.TrimSpace(raw))
		if source == "" {
			continue
		}
		if _, ok := allowedSourceSet[source]; !ok {
			return nil, fmt.Errorf("source %q is not one of: %s",
				raw, strings.Join(AllowedSources, ", "))
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		out = append(out, source)
	}
	return out, nil
}

// NormalizeCurrencies uppercases, trims, deduplicates, and validates fiat
// codes against commons/fiat.Enabled. Empty input yields an empty (non-nil) slice.
func NormalizeCurrencies(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if code == "" {
			continue
		}
		if !fiat.IsEnabled(code) {
			return nil, fmt.Errorf("currency %q is not one of: %s",
				raw, strings.Join(fiat.Enabled, ", "))
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	return out, nil
}
