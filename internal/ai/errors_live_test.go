//go:build netlive

package ai

import (
	"net"
	"testing"
)

// The live probe for the no-route case.
//
// The hermetic test builds the kernel's error shape by hand. This one dials
// for real, so it checks that the SHAPE we build is still what this host
// produces. It is opt-in (build tag: netlive) because on a host whose IPv6
// stack blackholes 2001:db8::1 instead of refusing it, the dial takes 75s and
// measures the HOST's routing rather than this classifier.
//
//	go test -tags netlive ./internal/ai/ -run TestLiveNoRouteDial
func TestLiveNoRouteDial(t *testing.T) {
	c, err := net.Dial("tcp", "[2001:db8::1]:80")
	if c != nil {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("the unroutable address dialed successfully; this host resolves it")
	}
	t.Logf("live no-route dial error: %v", err)
	if !IsNetworkOutage(err) {
		t.Fatalf("the live no-route error is not classified as an outage: %v", err)
	}
}
