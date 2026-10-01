//go:build !windows

package main

import "testing"

func TestBgSysProcAttrSetsID(t *testing.T) {
	attr := bgSysProcAttr()
	if attr == nil || !attr.Setsid {
		t.Fatalf("Setsid not set: %+v", attr)
	}
}