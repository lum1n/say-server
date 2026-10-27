package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/lum1n/say-v2/internal/core"
)

// raw sends a JSON request with an optional cookie and returns the response
// with its body read.
func (e *env) raw(method, path string, body any, cookie *http.Cookie) (*http.Response, []byte) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.ts.URL+"/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, data
}

func sessionCookie(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == TokenCookie && c.Value != "" {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 {
				t.Fatalf("cookie attributes: %+v", c)
			}
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", TokenCookie, res.Header["Set-Cookie"])
	return nil
}

func errCode(data []byte) string {
	var body struct{ Error apiError }
	json.Unmarshal(data, &body)
	return body.Error.Code
}

func TestSetupWizard(t *testing.T) {
	e := newEnv(t)
	e.srv.setupCode = "the-code"

	var status struct{ Needed, Open bool }
	if code := e.do("GET", "/setup", "", nil, &status); code != 200 || !status.Needed || !status.Open {
		t.Fatalf("setup status = %d %+v", code, status)
	}
	admin := map[string]string{"code": "the-code", "username": " Ada ", "display_name": "Ada L", "password": "correct horse"}

	bad := map[string]string{}
	for k, v := range admin {
		bad[k] = v
	}
	bad["code"] = "wrong"
	if res, data := e.raw("POST", "/setup", bad, nil); res.StatusCode != 403 || errCode(data) != "bad_setup_code" {
		t.Fatalf("wrong code = %d %s", res.StatusCode, data)
	}
	bad["code"], bad["password"] = "the-code", "short"
	if res, data := e.raw("POST", "/setup", bad, nil); res.StatusCode != 422 {
		t.Fatalf("short password = %d %s", res.StatusCode, data)
	}
	bad["password"], bad["username"] = "correct horse", "-ada"
	if res, data := e.raw("POST", "/setup", bad, nil); res.StatusCode != 422 {
		t.Fatalf("bad username = %d %s", res.StatusCode, data)
	}

	res, data := e.raw("POST", "/setup", admin, nil)
	if res.StatusCode != 201 {
		t.Fatalf("setup = %d %s", res.StatusCode, data)
	}
	var p wirePrincipal
	json.Unmarshal(data, &p)
	if !p.Admin || p.DisplayName != "Ada L" || p.Kind != core.PrincipalHuman {
		t.Fatalf("admin = %+v", p)
	}
	cookie := sessionCookie(t, res)
	res, data = e.raw("GET", "/me", nil, cookie)
	if res.StatusCode != 200 || !bytes.Contains(data, []byte(`"admin":true`)) {
		t.Fatalf("me with session = %d %s", res.StatusCode, data)
	}

	if res, data := e.raw("POST", "/setup", admin, nil); res.StatusCode != 409 || errCode(data) != "setup_done" {
		t.Fatalf("second setup = %d %s", res.StatusCode, data)
	}
	if code := e.do("GET", "/setup", "", nil, &status); code != 200 || status.Needed || status.Open {
		t.Fatalf("setup status after = %d %+v", code, status)
	}
}

func TestSetupWindow(t *testing.T) {
	// A node nobody claimed in time refuses setup, even with the code.
	e := newEnv(t)
	e.srv.setupCode = "the-code"
	e.srv.setupUntil = time.Now().Add(-time.Second)
	var status struct{ Needed, Open bool }
	if code := e.do("GET", "/setup", "", nil, &status); code != 200 || !status.Needed || status.Open {
		t.Fatalf("setup status = %d %+v", code, status)
	}
	body := map[string]string{"code": "the-code", "username": "ada", "password": "correct horse"}
	if res, data := e.raw("POST", "/setup", body, nil); res.StatusCode != 403 || errCode(data) != "setup_closed" {
		t.Fatalf("setup after the window = %d %s", res.StatusCode, data)
	}
	e.srv.setupUntil = time.Now().Add(time.Minute)
	if res, data := e.raw("POST", "/setup", body, nil); res.StatusCode != 201 {
		t.Fatalf("setup in the window = %d %s", res.StatusCode, data)
	}
}

func TestSetupWithoutCode(t *testing.T) {
	// A node started after setup has no code and refuses.
	e := newEnv(t)
	body := map[string]string{"code": "", "username": "ada", "password": "correct horse"}
	if res, data := e.raw("POST", "/setup", body, nil); res.StatusCode != 409 {
		t.Fatalf("setup without code = %d %s", res.StatusCode, data)
	}
}

func TestLoginLogoutAndPrincipals(t *testing.T) {
	e := newEnv(t)
	e.srv.setupCode = "c"
	res, data := e.raw("POST", "/setup", map[string]string{"code": "c", "username": "ada", "password": "correct horse"}, nil)
	if res.StatusCode != 201 {
		t.Fatalf("setup = %d %s", res.StatusCode, data)
	}
	admin := sessionCookie(t, res)

	// The admin adds a person and an agent.
	res, data = e.raw("POST", "/principals", map[string]string{"kind": "human", "username": "bob", "password": "hunter2hunter2"}, admin)
	if res.StatusCode != 201 {
		t.Fatalf("create human = %d %s", res.StatusCode, data)
	}
	res, data = e.raw("POST", "/principals", map[string]string{"kind": "human", "username": "BOB", "password": "hunter2hunter2"}, admin)
	if res.StatusCode != 409 || errCode(data) != "username_taken" {
		t.Fatalf("duplicate username = %d %s", res.StatusCode, data)
	}
	res, data = e.raw("POST", "/principals", map[string]string{"kind": "agent", "display_name": "Helper"}, admin)
	var agent struct {
		Principal wirePrincipal
		Token     string
	}
	if json.Unmarshal(data, &agent); res.StatusCode != 201 || agent.Token == "" || agent.Principal.Kind != core.PrincipalAgent {
		t.Fatalf("create agent = %d %s", res.StatusCode, data)
	}
	var me wirePrincipal
	if code := e.do("GET", "/me", agent.Token, nil, &me); code != 200 || me.ID != agent.Principal.ID {
		t.Fatalf("agent token = %d %+v", code, me)
	}

	for _, body := range []map[string]string{
		{"username": "bob", "password": "wrong password"},
		{"username": "nobody", "password": "hunter2hunter2"},
	} {
		if res, data := e.raw("POST", "/login", body, nil); res.StatusCode != 401 || errCode(data) != "invalid_credentials" {
			t.Fatalf("login %v = %d %s", body, res.StatusCode, data)
		}
	}
	res, data = e.raw("POST", "/login", map[string]string{"username": "Bob", "password": "hunter2hunter2"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("login = %d %s", res.StatusCode, data)
	}
	bob := sessionCookie(t, res)
	json.Unmarshal(data, &me)
	if me.Admin || me.DisplayName != "bob" {
		t.Fatalf("bob = %+v", me)
	}

	// Only admins create principals.
	if res, _ := e.raw("POST", "/principals", map[string]string{"kind": "agent", "display_name": "x"}, bob); res.StatusCode != 403 {
		t.Fatalf("non-admin create = %d", res.StatusCode)
	}

	res, _ = e.raw("POST", "/logout", nil, bob)
	if res.StatusCode != 204 {
		t.Fatalf("logout = %d", res.StatusCode)
	}
	if c := res.Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("logout cookie = %+v", c)
	}
	if res, _ := e.raw("GET", "/me", nil, bob); res.StatusCode != 401 {
		t.Fatalf("revoked session = %d", res.StatusCode)
	}
	if res, _ := e.raw("GET", "/me", nil, admin); res.StatusCode != 200 {
		t.Fatalf("other session after logout = %d", res.StatusCode)
	}
}

func TestListPrincipals(t *testing.T) {
	e := newEnv(t)
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	_, guestTok := e.user("guest", core.PrincipalGuest)

	for _, tok := range []string{adaTok, botTok} {
		var out struct{ Principals []wirePrincipal }
		if code := e.do("GET", "/principals", tok, nil, &out); code != 200 || len(out.Principals) != 3 {
			t.Fatalf("list = %d %+v", code, out)
		}
		ids := map[string]string{}
		for _, p := range out.Principals {
			ids[p.ID] = p.DisplayName
		}
		if ids[ada.ID] != "Ada" || ids[bot.ID] != "bot" {
			t.Fatalf("directory = %v", ids)
		}
	}
	if code := e.do("GET", "/principals", guestTok, nil, nil); code != 403 {
		t.Fatalf("guest list = %d", code)
	}
	if code := e.do("GET", "/principals", "", nil, nil); code != 401 {
		t.Fatalf("anonymous list = %d", code)
	}
}
