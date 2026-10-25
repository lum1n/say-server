package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiError is Slack's {"ok": false, "error": …}: the call was refused.
type apiError struct {
	Method string
	Code   string
}

func (e *apiError) Error() string { return "slack: " + e.Method + ": " + e.Code }

// response is the envelope of every Web API answer.
type response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (r response) result() response { return r }

// result lets call read ok and error from any response struct that
// embeds response.
type result interface{ result() response }

// maxRetryAfter caps how long one rate-limited call waits.
const maxRetryAfter = 2 * time.Minute

// call posts a Web API method with a token and decodes the answer into
// out. A rate-limited call (HTTP 429) waits as long as Slack asks and
// tries again; if ctx ends while it waits, the call surely did not happen
// and the error is an apiError "ratelimited".
func (c *conn) call(ctx context.Context, method, token string, params url.Values, out result) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+method, strings.NewReader(params.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := time.Duration(1) * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			wait = min(wait, maxRetryAfter)
			select {
			case <-ctx.Done():
				return &apiError{Method: method, Code: "ratelimited"}
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("slack: %s: HTTP %d", method, resp.StatusCode)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("slack: %s: %w", method, err)
		}
		if r := out.result(); !r.OK {
			return &apiError{Method: method, Code: r.Error}
		}
		return nil
	}
}
