// SPDX-License-Identifier: Apache-2.0

package costfmt

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/clientstate"

	"github.com/multigres/testkit/assert"
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
		got := Format(tc.usd, nil)
		assert.NewCollecting(t).Eq(tc.want, got, "Format(%v, nil) = %q, want", tc.usd, got)
	}
}

func TestFormat_Converts(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}
	if got, want := Format(1.0, cur), "$1.38 CAD"; got != want {
		t.Errorf("Format(1.0, cur) = %q, want %q", got, want)
	}
	// Sub-cent after conversion still keeps 4 decimals.
	got, want := Format(0.003, cur), "$0.0041 CAD"
	assert.NewCollecting(t).Eq(want, got, "Format(0.003, cur)")
}

// A zero or unset rate is "not configured" -- fall back to plain USD rather
// than converting by a meaningless factor.
func TestFormat_UnsetRateFallsBackToUSD(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD"} // Rate: 0
	got, want := Format(1.0, cur), "$1.00"
	assert.NewCollecting(t).Eq(want, got, "Format(1.0, cur)")
}

func TestToUSD_NoCurrency(t *testing.T) {
	assert.NewCollecting(t).Eq(12.5, ToUSD(12.5, nil), "ToUSD(12.5, nil)")
}

func TestToUSD_UnsetRatePassesThrough(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD"} // Rate: 0
	assert.NewCollecting(t).Eq(12.5, ToUSD(12.5, cur), "ToUSD(12.5, cur)")
}

func TestToUSD_ConvertsAndRoundTripsWithFormat(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}
	usd := ToUSD(1.38, cur)
	if diff := usd - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ToUSD(1.38, cur) = %v, want ~1.0", usd)
	}
	// Round-trips through Format: a local-currency amount converted to USD
	// and displayed back through Format should read as the original amount.
	got, want := Format(usd, cur), "$1.38 CAD"
	assert.NewCollecting(t).Eq(want, got, "Format(ToUSD(1.38, cur), cur)")
}

func TestToDisplay(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq(10.0, ToDisplay(10.0, nil), "ToDisplay(10, nil)")
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}
	if diff := ToDisplay(10.0, cur) - 13.8; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ToDisplay(10, cur) = %v, want ~13.8", ToDisplay(10.0, cur))
	}
	curZero := &clientstate.Currency{Code: "CAD", Rate: 0}
	c.Eq(10.0, ToDisplay(10.0, curZero), "ToDisplay(10, curZero)")
}
