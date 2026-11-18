package imapclient

import (
	"testing"
)

func TestXoauth2SASL(t *testing.T) {
	x := newXOAUTH2("user@x.com", "TOKEN")
	mech, ir, err := x.Start()
	if err != nil || mech != "XOAUTH2" {
		t.Fatalf("Start: %s %v", mech, err)
	}
	want := "user=user@x.com\x01auth=Bearer TOKEN\x01\x01"
	if string(ir) != want {
		t.Fatalf("initial response = %q", ir)
	}
	if resp, err := x.Next([]byte("challenge")); err != nil || len(resp) != 0 {
		t.Fatalf("Next = %q %v", resp, err)
	}
}
