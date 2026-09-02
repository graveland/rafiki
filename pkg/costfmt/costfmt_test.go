// SPDX-License-Identifier: Apache-2.0

package costfmt

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/clientstate"
)

func TestFormat_USD(t *testing.T) {
	for _, tc := range []struct {
		usd  float64
		want string
	}{
		{0, "-"},
		{0.004, "$0.0040"},
		{0.42, "$0.42"},
		{12.5, "$12.50"},
	} {
		if got := Format(tc.usd, nil); got != tc.want {
			t.Errorf("Format(%v, nil) = %q, want %q", tc.usd, got, tc.want)
		}
	}
}

func TestFormat_Converts(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}
	if got, want := Format(1.0, cur), "$1.38 CAD"; got != want {
		t.Errorf("Format(1.0, cur) = %q, want %q", got, want)
	}
	// Sub-cent after conversion still keeps 4 decimals.
	if got, want := Format(0.003, cur), "$0.0041 CAD"; got != want {
		t.Errorf("Format(0.003, cur) = %q, want %q", got, want)
	}
}

// A zero or unset rate is "not configured" -- fall back to plain USD rather
// than converting by a meaningless factor.
func TestFormat_UnsetRateFallsBackToUSD(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD"} // Rate: 0
	if got, want := Format(1.0, cur), "$1.00"; got != want {
		t.Errorf("Format(1.0, cur) = %q, want %q", got, want)
	}
}

func TestToUSD_NoCurrency(t *testing.T) {
	if got := ToUSD(12.5, nil); got != 12.5 {
		t.Errorf("ToUSD(12.5, nil) = %v, want 12.5", got)
	}
}

func TestToUSD_UnsetRatePassesThrough(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD"} // Rate: 0
	if got := ToUSD(12.5, cur); got != 12.5 {
		t.Errorf("ToUSD(12.5, cur) = %v, want 12.5 (unset rate passes through)", got)
	}
}

func TestToUSD_ConvertsAndRoundTripsWithFormat(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}
	usd := ToUSD(1.38, cur)
	if diff := usd - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ToUSD(1.38, cur) = %v, want ~1.0", usd)
	}
	// Round-trips through Format: a local-currency amount converted to USD
	// and displayed back through Format should read as the original amount.
	if got, want := Format(usd, cur), "$1.38 CAD"; got != want {
		t.Errorf("Format(ToUSD(1.38, cur), cur) = %q, want %q", got, want)
	}
}
