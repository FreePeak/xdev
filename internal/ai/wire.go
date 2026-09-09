package ai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// wireHTTPClient is the shared transport for every wire adapter: no overall
// timeout (streams legitimately run for minutes), but bounded dial, TLS
// handshake (30s) and response-header (5m) phases so dead endpoints fail.
var wireHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	},
}

// wirePost builds and sends one streaming POST. It returns the 2xx response,
// or a descriptive pre-flight error (status plus up to 4 KiB of body).
func wirePost(ctx context.Context, hc *http.Client, url string, headers map[string]string, body []byte, api string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", api, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", api, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, &HTTPError{API: api, Status: resp.StatusCode, Body: wireErrBody(resp.Body)}
	}
	return resp, nil
}

// wireErrBody reads up to 4 KiB of an error body and flattens it to one line.
func wireErrBody(r io.Reader) string {
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(r, buf)
	return strings.Join(strings.Fields(string(buf[:n])), " ")
}

// reasoningEffort maps a thinking budget to the OpenAI reasoning-effort tier.
func reasoningEffort(tokens int) string {
	switch {
	case tokens <= 2048:
		return "low"
	case tokens <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// emptyJSONObjectArgs normalizes empty tool-call arguments to "{}": providers
// reject an empty string in the arguments field.
func emptyJSONObjectArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}
