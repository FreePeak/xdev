package ai

import "testing"

// Test404PageNotFoundFromGateway verifies that an HTTP 404 with body
// "404 page not found" (returned by the onegw gateway when its upstream
// retires a model) classifies as ClassTransient, not ClassUnknown.
// Same contract as upstream_error: the local route is fine, an upstream
// target is down, so the recovery ladder should retry and fail over
// instead of ending the run.
func Test404PageNotFoundFromGateway(t *testing.T) {
	err := &HTTPError{API: "openai-completions", Status: 404, Body: "404 page not found"}
	if got := Classify(err); got != ClassTransient {
		t.Fatalf("Classify(404 page not found) = %v, want ClassTransient", got)
	}
	if !Retriable(err) {
		t.Fatal("404 page not found must be retriable")
	}
}
