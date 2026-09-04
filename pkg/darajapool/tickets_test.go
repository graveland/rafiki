// SPDX-License-Identifier: Apache-2.0

package darajapool

import "testing"

func TestTicketRedeemsOnce(t *testing.T) {
	r := NewRegistry()
	tk, err := r.MintTicket("c1")
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := r.RedeemTicket(tk); !ok || id != "c1" {
		t.Fatalf("first redeem = (%q, %v), want (c1, true)", id, ok)
	}
	if _, ok := r.RedeemTicket(tk); ok {
		t.Error("a spent ticket redeemed a second time")
	}
}

func TestCredentialIsBoundToItsChild(t *testing.T) {
	r := NewRegistry()
	cred, err := r.IssueCredential("c1")
	if err != nil {
		t.Fatal(err)
	}
	if !r.CheckCredential(cred, "c1") {
		t.Error("the issuing child was rejected")
	}
	if r.CheckCredential(cred, "c2") {
		t.Error("a credential issued for c1 authenticated c2")
	}
	if r.CheckCredential("not-a-credential", "c1") {
		t.Error("an unknown credential was accepted")
	}
}

func TestEmptyCredentialIsRefused(t *testing.T) {
	r := NewRegistry()
	if r.CheckCredential("", "c1") {
		t.Error("an empty credential authenticated a child")
	}
	if _, _ = r.IssueCredential("c1"); r.CheckCredential("", "c1") {
		t.Error("an empty credential authenticated a child that HAS one")
	}
}

func TestForgetRevokesEverything(t *testing.T) {
	r := NewRegistry()
	tk, _ := r.MintTicket("c1")
	cred, _ := r.IssueCredential("c1")
	r.Forget("c1")
	if _, ok := r.RedeemTicket(tk); ok {
		t.Error("a forgotten child's ticket still redeems")
	}
	if r.CheckCredential(cred, "c1") {
		t.Error("a forgotten child's credential still authenticates")
	}
}
