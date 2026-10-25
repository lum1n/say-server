package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiError is a 4xx answer: Discord refused the call, so it surely did
// not happen.
type apiError struct {
	Method  string
	Route   string // the path, without webhook tokens
	Status  int
	Code    int // Discord's JSON error code
	Message string
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("discord: %s %s: HTTP %d", e.Method, e.Route, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// status returns the HTTP status of a refusal, or 0.
func status(err error) int {
	if e, ok := errors.AsType[*apiError](err); ok {
		return e.Status
	}
	return 0
}

const (
	// maxAttempts bounds the tries of one rate-limited call.
	maxAttempts = 5
	// maxRetryAfter caps how long one rate-limited call waits.
	maxRetryAfter = time.Minute
	userAgent     = "DiscordBot (https://github.com/lum1n/say-v2, 1)"
)

// do calls the REST API with a JSON body and decodes the answer into out.
// A 429 waits as long as Discord asks and tries again, a few times; when
// it gives up, or ctx ends while it waits, the error is an apiError with
// status 429.
func (c *conn) do(ctx context.Context, method, path string, body, out any) error {
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return err
		}
	}
	route := redact(path)
	key := method + " " + bucket(route)
	limited := &apiError{Method: method, Route: route, Status: http.StatusTooManyRequests, Message: "rate limited"}
	for attempt := 1; ; attempt++ {
		if !c.waitBucket(ctx, key) {
			return limited
		}
		req, err := http.NewRequestWithContext(ctx, method, c.api+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		if data != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		// Webhook calls carry their token in the path.
		if !strings.HasPrefix(path, "/webhooks/") {
			req.Header.Set("Authorization", "Bot "+c.cfg.BotToken)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			// The URL may hold a webhook token; keep it out of errors.
			if ue, ok := errors.AsType[*url.Error](err); ok {
				err = ue.Err
			}
			return fmt.Errorf("discord: %s %s: %w", method, route, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("discord: %s %s: %w", method, route, err)
		}
		c.noteBucket(key, resp.Header)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			if attempt >= maxAttempts {
				return limited
			}
			wait := retryAfter(resp.Header, raw)
			if resp.Header.Get("X-RateLimit-Global") == "true" {
				c.mu.Lock()
				c.global = time.Now().Add(wait)
				c.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return limited
			case <-time.After(wait):
			}
			continue
		case resp.StatusCode >= 500:
			return fmt.Errorf("discord: %s %s: HTTP %d", method, route, resp.StatusCode)
		case resp.StatusCode >= 400:
			e := &apiError{Method: method, Route: route, Status: resp.StatusCode}
			var r struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &r) == nil {
				e.Code, e.Message = r.Code, r.Message
			}
			return e
		}
		if out != nil && len(raw) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("discord: %s %s: %w", method, route, err)
			}
		}
		return nil
	}
}

// retryAfter reads how long a 429 asks to wait, from the body or header.
func retryAfter(h http.Header, body []byte) time.Duration {
	var r struct {
		RetryAfter float64 `json:"retry_after"`
	}
	secs := 1.0
	if json.Unmarshal(body, &r) == nil && r.RetryAfter > 0 {
		secs = r.RetryAfter
	} else if s, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && s > 0 {
		secs = s
	}
	return min(time.Duration(secs*float64(time.Second)), maxRetryAfter)
}

// noteBucket remembers a route that has used up its requests until the
// bucket resets, so the next call waits instead of getting a 429.
func (c *conn) noteBucket(key string, h http.Header) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	s, err := strconv.ParseFloat(h.Get("X-RateLimit-Reset-After"), 64)
	if err != nil || s <= 0 {
		return
	}
	c.mu.Lock()
	c.buckets[key] = time.Now().Add(min(time.Duration(s*float64(time.Second)), maxRetryAfter))
	c.mu.Unlock()
}

// waitBucket waits until a route and the global limit allow a call;
// false means ctx ended first.
func (c *conn) waitBucket(ctx context.Context, key string) bool {
	c.mu.Lock()
	until := c.buckets[key]
	if c.global.After(until) {
		until = c.global
	}
	c.mu.Unlock()
	d := time.Until(until)
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// redact drops the query and a webhook's token from a path.
func redact(path string) string {
	path, _, _ = strings.Cut(path, "?")
	if rest, ok := strings.CutPrefix(path, "/webhooks/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) > 1 {
			parts[1] = "…"
		}
		return "/webhooks/" + strings.Join(parts, "/")
	}
	return path
}

// bucket groups the routes of one message together: Discord limits them
// per channel or webhook, not per message.
func bucket(route string) string {
	if i := strings.Index(route, "/messages/"); i >= 0 {
		return route[:i] + "/messages/:id"
	}
	return route
}
