package fiat

import "testing"

func TestNormalize(t *testing.T) {
	if got, ok := Normalize(" chf "); !ok || got != "CHF" {
		t.Fatalf("Normalize(CHF) = (%q, %v)", got, ok)
	}
	if got, ok := Normalize(""); !ok || got != "EUR" {
		t.Fatalf("Normalize(empty) = (%q, %v)", got, ok)
	}
	if _, ok := Normalize("JPY"); ok {
		t.Fatal("unexpectedly accepted unsupported currency")
	}
}

func TestDEXCapabilities(t *testing.T) {
	for _, currency := range Enabled {
		if !SupportsDEX("bisq", currency) || !SupportsDEX("hodlhodl", currency) ||
			!SupportsDEX("mostro", currency) {
			t.Fatalf("expected Bisq, HodlHodl and Mostro to support %s", currency)
		}
	}
	if SupportsDEX("peachbitcoin", "USD") || !SupportsDEX("peachbitcoin", "CHF") {
		t.Fatal("unexpected Peach capability matrix")
	}
	if !SupportsDEX("robosats", "CAD") || !SupportsDEX("robosats", "EUR") {
		t.Fatal("unexpected RoboSats capability matrix")
	}
}
