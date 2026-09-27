// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestTicketRedeemsOnce(t *testing.T) {
	c := assert.NewCollecting(t)
	r := NewRegistry()
	tk, err := r.MintTicket("c1")
	c.Require().NoError(err)
	if id, ok := r.RedeemTicket(tk); !ok || id != "c1" {
		t.Fatalf("first redeem = (%q, %v), want (c1, true)", id, ok)
	}
	_, ok := r.RedeemTicket(tk)
	c.False(ok, "a spent ticket redeemed a second time")
}

func TestCredentialIsBoundToItsChild(t *testing.T) {
	c := assert.NewCollecting(t)
	r := NewRegistry()
	cred, err := r.IssueCredential("c1")
	c.Require().NoError(err)
	c.True(r.CheckCredential(cred, "c1"), "the issuing child was rejected")
	c.False(r.CheckCredential(cred, "c2"), "a credential issued for c1 authenticated c2")
	c.False(r.CheckCredential("not-a-credential", "c1"), "an unknown credential was accepted")
}

func TestEmptyCredentialIsRefused(t *testing.T) {
	r := NewRegistry()
	assert.NewCollecting(t).False(r.CheckCredential("", "c1"), "an empty credential authenticated a child")
	if _, _ = r.IssueCredential("c1"); r.CheckCredential("", "c1") {
		t.Error("an empty credential authenticated a child that HAS one")
	}
}

func TestForgetRevokesEverything(t *testing.T) {
	c := assert.NewCollecting(t)
	r := NewRegistry()
	tk, _ := r.MintTicket("c1")
	cred, _ := r.IssueCredential("c1")
	r.Forget("c1")
	_, ok := r.RedeemTicket(tk)
	c.False(ok, "a forgotten child's ticket still redeems")
	c.False(r.CheckCredential(cred, "c1"), "a forgotten child's credential still authenticates")
}
