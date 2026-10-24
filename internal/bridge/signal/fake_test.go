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
//	account         the linked number; startLink is needed without it
//	scanned         lets a waiting finishLink finish, as a phone would
//	push-*.json     params of a "receive" notification to send, in order
//	requests.jsonl  every request the fake got
//	die             makes it complain on stderr and exit
const fakeEnv = "SAY_FAKE_SIGNAL_CLI"

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

func (f *fake) log(line []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lf, err := os.OpenFile(filepath.Join(f.dir, "requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
		if raw, err := os.ReadFile(filepath.Join(f.dir, "account")); err == nil {
			list = append(list, map[string]string{"number": strings.TrimSpace(string(raw))})
		}
		result = list
	case "startLink":
		result = map[string]string{"deviceLinkUri": "sgnl://linkdevice?uuid=fake&pub_key=fake"}
	case "finishLink":
		for !f.exists("scanned") {
			time.Sleep(5 * time.Millisecond)
		}
		os.WriteFile(filepath.Join(f.dir, "account"), []byte(fakeNumber), 0o600)
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
