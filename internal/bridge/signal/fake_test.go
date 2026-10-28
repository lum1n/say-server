package signal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The test binary doubles as a fake signal-cli: run with fakeEnv set, it
// speaks signal-cli's JSON-RPC on stdin and stdout. Tests steer it through
// files in its data directory:
//
//	data/account    the linked number; startLink is needed without it
//	scanned         lets a waiting finishLink finish, as a phone would
//	outdated        makes a waiting finishLink fail as Signal fails an old signal-cli
//	push-*.json     params of a "receive" notification to send, in order
//	requests.jsonl  every request the fake got
//	die             makes it complain on stderr and exit
//	offline         makes unregister fail as without a network
//
// Run with "-a NUMBER unregister" or "deleteLocalAccountData" instead of
// jsonRpc, it logs the command to requests.jsonl; the latter deletes the
// account file. Like signal-cli it keeps the account under data/.
const fakeEnv = "SAY_FAKE_SIGNAL_CLI"

// fakeLogEnv, if set, names a file the one-off commands are logged to as
// well, for directories that are gone by the time the test looks.
const fakeLogEnv = "SAY_FAKE_SIGNAL_CLI_LOG"

const (
	fakeNumber = "+4700000001"
	fakeUUID   = "u-me"
)

func fakeSignalCLI() {
	var dir string
	for i, a := range os.Args {
		if a == "--config" && i+1 < len(os.Args) {
			dir = os.Args[i+1]
		}
	}
	if dir != "" && fakeCommand(dir) {
		return
	}
	if dir == "" || os.Args[len(os.Args)-1] != "jsonRpc" {
		fmt.Fprintln(os.Stderr, "fake signal-cli: bad arguments", os.Args)
		os.Exit(1)
	}
	f := &fake{dir: dir, out: bufio.NewWriter(os.Stdout)}
	go f.poll()
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req wire
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		f.log(sc.Bytes())
		go f.handle(req)
	}
	os.Exit(0)
}

// fakeCommand runs a one-off command, if the arguments are one.
func fakeCommand(dir string) bool {
	var account, command string
	for i, a := range os.Args {
		if a == "-a" && i+2 < len(os.Args) {
			account, command = os.Args[i+1], os.Args[i+2]
		}
	}
	if command != "unregister" && command != "deleteLocalAccountData" {
		return false
	}
	f := &fake{dir: dir}
	line, _ := json.Marshal(map[string]string{"command": command, "account": account})
	f.log(line)
	if p := os.Getenv(fakeLogEnv); p != "" {
		(&fake{dir: filepath.Dir(p)}).logTo(filepath.Base(p), line)
	}
	switch {
	case command == "unregister" && f.exists("offline"):
		fmt.Fprintln(os.Stderr, "ERROR network is unreachable")
		os.Exit(3)
	case command == "deleteLocalAccountData":
		os.Remove(filepath.Join(dir, "data", "account"))
	}
	os.Exit(0)
	return true
}

type fake struct {
	dir string

	mu   sync.Mutex
	out  *bufio.Writer
	sent int
}

func (f *fake) write(m any) {
	line, _ := json.Marshal(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out.Write(append(line, '\n'))
	f.out.Flush()
}

func (f *fake) log(line []byte) { f.logTo("requests.jsonl", line) }

func (f *fake) logTo(name string, line []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lf, err := os.OpenFile(filepath.Join(f.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer lf.Close()
	lf.Write(append(append([]byte(nil), line...), '\n'))
}

func (f *fake) exists(name string) bool {
	_, err := os.Stat(filepath.Join(f.dir, name))
	return err == nil
}

// poll sends pushed notifications and dies on request.
func (f *fake) poll() {
	for {
		if f.exists("die") {
			fmt.Fprintln(os.Stderr, "INFO starting\nERROR boom: database is locked")
			os.Exit(3)
		}
		names, _ := filepath.Glob(filepath.Join(f.dir, "push-*.json"))
		sort.Strings(names)
		for _, n := range names {
			raw, err := os.ReadFile(n)
			if err != nil {
				continue
			}
			f.write(map[string]any{"jsonrpc": "2.0", "method": "receive", "params": json.RawMessage(raw)})
			os.Remove(n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fake) handle(req wire) {
	var p struct {
		Account   string   `json:"account"`
		Recipient []string `json:"recipient"`
		GroupID   string   `json:"groupId"`
	}
	json.Unmarshal(req.Params, &p)
	var result any
	var rerr *rpcError
	switch req.Method {
	case "listAccounts":
		list := []map[string]string{}
		if raw, err := os.ReadFile(filepath.Join(f.dir, "data", "account")); err == nil {
			list = append(list, map[string]string{"number": strings.TrimSpace(string(raw))})
		}
		result = list
	case "startLink":
		result = map[string]string{"deviceLinkUri": "sgnl://linkdevice?uuid=fake&pub_key=fake"}
	case "finishLink":
		for !f.exists("scanned") && !f.exists("outdated") {
			time.Sleep(5 * time.Millisecond)
		}
		if f.exists("outdated") {
			rerr = &rpcError{Code: -3, Message: "Link request error: StatusCode: 409 (MissingCapabilitiesException)"}
			break
		}
		os.MkdirAll(filepath.Join(f.dir, "data"), 0o700)
		os.WriteFile(filepath.Join(f.dir, "data", "account"), []byte(fakeNumber), 0o600)
		result = map[string]string{"number": fakeNumber}
	case "listContacts":
		result = []map[string]any{
			{"number": "+4711111111", "uuid": "u-bob", "name": "Bob"},
			{"number": fakeNumber, "uuid": fakeUUID, "name": ""},
		}
	case "listGroups":
		result = []map[string]any{{"id": "g1", "name": "Friends"}}
	case "send":
		switch {
		case len(p.Recipient) == 1 && p.Recipient[0] == "u-gone":
			rerr = &rpcError{Code: -1, Message: "Failed to send message",
				Data: json.RawMessage(`{"response":{"timestamp":1,"results":[{"type":"UNREGISTERED_FAILURE"}]}}`)}
		case p.GroupID == "g-partial":
			// Some members got it, one is no longer on Signal.
			rerr = &rpcError{Code: -1, Message: "Failed to send message",
				Data: json.RawMessage(`{"response":{"timestamp":1700000000999,"results":[{"type":"SUCCESS"},{"type":"UNREGISTERED_FAILURE"}]}}`)}
		case len(p.Recipient) == 1 && p.Recipient[0] == "u-flaky":
			rerr = &rpcError{Code: -1, Message: "Failed to send message",
				Data: json.RawMessage(`{"response":{"timestamp":1,"results":[{"type":"NETWORK_FAILURE"}]}}`)}
		default:
			f.mu.Lock()
			f.sent++
			ts := 1700000000000 + int64(f.sent)
			f.mu.Unlock()
			result = map[string]any{"timestamp": ts, "results": []map[string]string{{"type": "SUCCESS"}}}
		}
	case "sendReaction":
		result = map[string]any{"timestamp": 1700000000500, "results": []map[string]string{{"type": "SUCCESS"}}}
	default:
		rerr = &rpcError{Code: -32601, Message: "Method not implemented"}
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	f.write(resp)
}
