package telegrambot

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiError is Telegram's {"ok": false, …}: the call was refused.
type apiError struct {
	Method      string
	Code        int
	Description string
	// MigrateTo is set when a group became a supergroup with a new id.
	MigrateTo int64
}

func (e *apiError) Error() string {
	s := "telegram bot: " + e.Method + ": " + cmp.Or(e.Description, fmt.Sprintf("error %d", e.Code))
	if e.MigrateTo != 0 {
		s += fmt.Sprintf(" (the group is now the supergroup %d; link that id instead)", e.MigrateTo)
	}
	return s
}

// refused reports whether the call surely did not happen: Telegram
// answered with a client error.
func (e *apiError) refused() bool { return e.Code >= 400 && e.Code < 500 }

// response is the envelope of every Bot API answer.
type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter      int   `json:"retry_after"`
		MigrateToChatID int64 `json:"migrate_to_chat_id"`
	} `json:"parameters"`
}

const (
	// maxRetryAfter caps how long one rate-limited call waits.
	maxRetryAfter = 2 * time.Minute
	// maxAttempts bounds the tries of a rate-limited call.
	maxAttempts = 5
	// callTimeout is how long one HTTP request may take, but getUpdates.
	callTimeout = 30 * time.Second
)

// call posts a Bot API method with params as JSON and decodes the result
// into out. A rate-limited call (429) waits as long as Telegram asks and
// tries again, a few times; if ctx ends while it waits, the call surely
// did not happen and the error is the apiError.
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	timeout := callTimeout
	if method == "getUpdates" {
		timeout = time.Duration(c.poll)*time.Second + 20*time.Second
	}
	for attempt := 1; ; attempt++ {
		status, data, err := c.post(ctx, method, body, timeout)
		if err != nil {
			return err
		}
		var r response
		if err := json.Unmarshal(data, &r); err != nil {
			if status >= 400 && status < 500 {
				return &apiError{Method: method, Code: status, Description: fmt.Sprintf("HTTP %d", status)}
			}
			return fmt.Errorf("telegram bot: %s: HTTP %d", method, status)
		}
		if r.OK {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(r.Result, out); err != nil {
				return fmt.Errorf("telegram bot: %s: %w", method, err)
			}
			return nil
		}
		e := &apiError{Method: method, Code: cmp.Or(r.ErrorCode, status), Description: r.Description}
		wait := time.Second
		if p := r.Parameters; p != nil {
			e.MigrateTo = p.MigrateToChatID
			if p.RetryAfter > 0 {
				wait = time.Duration(p.RetryAfter) * time.Second
			}
		}
		if e.Code != http.StatusTooManyRequests || attempt >= maxAttempts {
			return e
		}
		select {
		case <-ctx.Done():
			return e
		case <-time.After(min(wait, maxRetryAfter)):
		}
	}
}

// post makes one request and returns the status and body.
func (c *conn) post(ctx context.Context, method string, body []byte, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"bot"+c.cfg.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return 0, nil, c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, c.redact(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, c.redact(err)
	}
	return resp.StatusCode, data, nil
}

// redact takes the token out of an error: it is in the URL's path, and
// net/http's errors include the URL.
func (c *conn) redact(err error) error {
	if c.cfg.Token == "" {
		return err
	}
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = &url.Error{Op: ue.Op, URL: strings.ReplaceAll(ue.URL, c.cfg.Token, "<token>"), Err: ue.Err}
	}
	if strings.Contains(err.Error(), c.cfg.Token) {
		return errors.New(strings.ReplaceAll(err.Error(), c.cfg.Token, "<token>"))
	}
	return err
}
