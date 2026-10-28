package matrix

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

// apiError is an error answer of the homeserver.
type apiError struct {
	Status     int    `json:"-"`
	Code       string `json:"errcode"`
	Message    string `json:"error"`
	RetryAfter int64  `json:"retry_after_ms"`
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("matrix: HTTP %d", e.Status)
	}
	return fmt.Sprintf("matrix: %s: %s", e.Code, e.Message)
}

// isCode reports whether err is an answer of the homeserver with errcode
// code.
func isCode(err error, code string) bool {
	var e *apiError
	return errors.As(err, &e) && e.Code == code
}

// isAbsent reports whether err says that a thing is not there, or not
// visible to us, which a lookup treats alike.
func isAbsent(err error) bool {
	var e *apiError
	return errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Status == http.StatusForbidden)
}

// isRefusal reports whether err is a 4xx answer: the homeserver surely
// did not do what was asked.
func isRefusal(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500
}

// path joins the segments of a client-server API path, escaping each,
// since room ids and user ids hold characters like '!', '@' and ':'.
func path(segments ...string) string {
	var b strings.Builder
	b.WriteString("/_matrix/client/v3")
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// Pauses between tries of a request; tests shorten them.
var (
	retryPause    = time.Second
	maxRetryAfter = 30 * time.Second
)

// requestTimeout bounds a request when the caller set no deadline.
const requestTimeout = time.Minute

// do sends a request to the homeserver and decodes its JSON answer into
// out. A rate limit is waited out. A GET or a PUT, which the API makes
// idempotent, is also tried again after a network error or a 5xx.
func (c *conn) do(ctx context.Context, method, p string, query url.Values, body, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	var payload []byte
	ctype := ""
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
		ctype = "application/json"
	}
	return c.doRaw(ctx, method, p, query, ctype, payload, out)
}

// doRaw is do with a body of any type.
func (c *conn) doRaw(ctx context.Context, method, p string, query url.Values, ctype string, payload []byte, out any) error {
	u := c.cfg.Homeserver + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	idempotent := method == http.MethodGet || method == http.MethodPut
	var err error
	for try := 0; try < 4; try++ {
		var wait time.Duration
		wait, err = c.try(ctx, method, u, ctype, payload, out)
		if err == nil {
			return nil
		}
		var e *apiError
		switch {
		case errors.As(err, &e) && e.Status == http.StatusTooManyRequests:
			wait = min(max(wait, retryPause), maxRetryAfter)
		case errors.As(err, &e) && e.Status < 500:
			return err
		case !idempotent || ctx.Err() != nil:
			return err
		default:
			wait = retryPause << try
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
	return err
}

// try sends a request once. With a rate limit it also returns how long
// the homeserver asks to wait.
func (c *conn) try(ctx context.Context, method, u, ctype string, payload []byte, out any) (time.Duration, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.roundTrip(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 300 {
		e, wait := answerError(resp, data)
		return wait, e
	}
	if out == nil {
		return 0, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return 0, fmt.Errorf("matrix: bad answer to %s: %w", method, err)
	}
	return 0, nil
}

// roundTrip sends a request with the session's token.
func (c *conn) roundTrip(req *http.Request) (*http.Response, error) {
	if token := c.token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.http.Do(req)
}

// answerError is the error of a failed answer, and how long a rate limit
// asks to wait.
func answerError(resp *http.Response, data []byte) (*apiError, time.Duration) {
	e := &apiError{Status: resp.StatusCode}
	json.Unmarshal(data, e)
	wait := time.Duration(e.RetryAfter) * time.Millisecond
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		wait = time.Duration(s) * time.Second
	}
	return e, wait
}
