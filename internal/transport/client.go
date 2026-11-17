// Package transport calls participants over HTTP and classifies the result
// into three buckets: definitive success, definitive failure, or unknown.
//
// "Unknown" is the interesting case for a distributed transaction: the
// participant may have applied the operation, may not have, or may have
// applied it and then lost the response. The engine must retry, and the
// participant's idempotency key makes the retry safe.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"crossborder/dtx/wire"
)

// Verdict is the classified result of one call.
type Verdict int

const (
	Applied  Verdict = iota // definitively applied (or idempotently already applied)
	Rejected                // definitively not applied, do not retry
	Unknown                 // may or may not have applied; retry
)

func (v Verdict) String() string {
	switch v {
	case Applied:
		return "OK"
	case Rejected:
		return "REJECTED"
	default:
		return "UNKNOWN"
	}
}

// Result is a classified call result.
type Result struct {
	Verdict Verdict
	Code    string
	Message string
	Outcome string
	Dur     time.Duration
	// Empty is true when a cancel/compensate was answered "nothing to do",
	// which is how participants report an empty rollback.
	Empty bool
}

// Client calls participants.
type Client struct {
	http *http.Client
}

// New builds a client. The per-call timeout is supplied by the caller via
// context, so this timeout is deliberately generous; it only bounds
// connection-level pathologies.
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 128,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// Call invokes one participant operation. timeout is the whole-call budget.
func (c *Client) Call(baseURL, path string, req *wire.Request, timeout time.Duration) Result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return Result{Verdict: Rejected, Code: wire.CodeInternal, Message: err.Error(), Dur: time.Since(start)}
	}
	url := strings.TrimRight(baseURL, "/") + path
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Result{Verdict: Rejected, Code: wire.CodeInternal, Message: err.Error(), Dur: time.Since(start)}
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hreq)
	if err != nil {
		return unknownFromErr(err, time.Since(start))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return unknownFromErr(err, time.Since(start))
	}
	dur := time.Since(start)

	if resp.StatusCode >= 500 {
		// A 5xx says nothing about whether the write landed.
		return Result{Verdict: Unknown, Code: fmt.Sprintf("HTTP_%d", resp.StatusCode), Message: truncate(string(raw)), Dur: dur}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return Result{Verdict: Unknown, Code: fmt.Sprintf("HTTP_%d", resp.StatusCode), Dur: dur}
	}
	if resp.StatusCode >= 400 {
		return Result{Verdict: Rejected, Code: fmt.Sprintf("HTTP_%d", resp.StatusCode), Message: truncate(string(raw)), Dur: dur}
	}

	var wr wire.Response
	if err := json.Unmarshal(raw, &wr); err != nil {
		return Result{Verdict: Unknown, Code: "BAD_RESPONSE", Message: truncate(string(raw)), Dur: dur}
	}
	if wr.OK {
		return Result{Verdict: Applied, Code: wr.Code, Message: wr.Message, Outcome: wr.Outcome,
			Dur: dur, Empty: wr.Outcome == "EMPTY" || wr.Code == wire.CodeAlreadyCanceled}
	}
	if wr.Retryable {
		return Result{Verdict: Unknown, Code: wr.Code, Message: wr.Message, Outcome: wr.Outcome, Dur: dur}
	}
	return Result{Verdict: Rejected, Code: wr.Code, Message: wr.Message, Outcome: wr.Outcome, Dur: dur,
		Empty: wr.Code == wire.CodeAlreadyCanceled && wr.Outcome == "EMPTY"}
}

func unknownFromErr(err error, dur time.Duration) Result {
	code := "NETWORK"
	msg := err.Error()
	var ne net.Error
	if ok := asNetErr(err, &ne); ok && ne.Timeout() {
		code = "TIMEOUT"
	}
	if strings.Contains(msg, "context deadline exceeded") {
		code = "TIMEOUT"
	}
	return Result{Verdict: Unknown, Code: code, Message: truncate(msg), Dur: dur}
}

func asNetErr(err error, target *net.Error) bool {
	for err != nil {
		if ne, ok := err.(net.Error); ok {
			*target = ne
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// FetchJSON is a small helper for metrics/health endpoints.
func (c *Client) FetchJSON(url string, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
