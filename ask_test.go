package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeSession = "11111111-1111-4111-8111-111111111111"

// fakeClaude writes a script that stands in for Claude Code: it reads the
// question from stdin and prints stream-json lines the way print mode does.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const answeringClaude = `q=$(cat)
case "$*" in *"--resume ` + fakeSession + `"*) q="resumed $q";; esac
echo '{"type":"system","subtype":"init","session_id":"` + fakeSession + `"}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"secret"}}]}}'
echo 'not json'
echo '{"type":"result","subtype":"success","is_error":false,"result":"answer to '"$q"'","session_id":"` + fakeSession + `","total_cost_usd":0.25}'
`

func startServer(t *testing.T, cfg serveConfig) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	journal := &bytes.Buffer{}
	cfg.workspace = t.TempDir()
	if cfg.outbox == "" {
		cfg.outbox = t.TempDir()
	}
	if cfg.inbox == "" {
		cfg.inbox = t.TempDir()
	}
	cfg.journal = journal
	if cfg.maxParallel == 0 {
		cfg.maxParallel = 2
	}
	if cfg.timeout == 0 {
		cfg.timeout = 10 * time.Second
	}
	if cfg.heartbeat == 0 {
		cfg.heartbeat = time.Minute
	}
	ts := httptest.NewServer(newServer(cfg).handler())
	t.Cleanup(ts.Close)
	return ts, journal
}

func ask(t *testing.T, url string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = runAsk(append([]string{"--server", url}, args...), strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestAskAndFollowUp(t *testing.T) {
	ts, journal := startServer(t, serveConfig{claude: fakeClaude(t, answeringClaude)})

	out, progress, err := ask(t, ts.URL, "how", "does", "it", "work")
	if err != nil {
		t.Fatal(err)
	}
	if want := "answer to how does it work\n\nthread: " + fakeSession + "\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if !strings.Contains(progress, "using Grep") || strings.Contains(progress, "secret") {
		t.Errorf("progress = %q, want the tool name and no tool input", progress)
	}

	out, _, err = ask(t, ts.URL, "--thread", fakeSession, "and away")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "answer to resumed and away\n") {
		t.Errorf("follow-up stdout = %q", out)
	}

	lines := strings.Split(strings.TrimSpace(journal.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal has %d lines, want 2", len(lines))
	}
	var entry journalEntry
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Who != "anonymous" || !entry.FollowUp || entry.Outcome != eventAnswer || entry.CostUSD != 0.25 {
		t.Errorf("journal entry = %+v", entry)
	}
}

func TestClaudeFailureIsReported(t *testing.T) {
	ts, _ := startServer(t, serveConfig{claude: fakeClaude(t, "cat >/dev/null; echo boom >&2; exit 3\n")})
	_, _, err := ask(t, ts.URL, "anything")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want Claude's stderr", err)
	}
}

func TestErrorResultKeepsThread(t *testing.T) {
	script := `cat >/dev/null
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"` + fakeSession + `"}'
`
	ts, _ := startServer(t, serveConfig{claude: fakeClaude(t, script)})
	out, _, err := ask(t, ts.URL, "anything")
	if err == nil || !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out, "thread: "+fakeSession) {
		t.Errorf("stdout = %q, want the thread id", out)
	}
}

func TestTimeout(t *testing.T) {
	ts, _ := startServer(t, serveConfig{
		claude:  fakeClaude(t, "exec sleep 30\n"),
		timeout: 200 * time.Millisecond,
	})
	_, _, err := ask(t, ts.URL, "anything")
	if err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Errorf("err = %v", err)
	}
}

func TestHeartbeatWhileClaudeIsSilent(t *testing.T) {
	script := `cat >/dev/null
sleep 1
echo '{"type":"result","subtype":"success","result":"done","session_id":"` + fakeSession + `"}'
`
	ts, _ := startServer(t, serveConfig{claude: fakeClaude(t, script), heartbeat: 100 * time.Millisecond})
	_, progress, err := ask(t, ts.URL, "anything")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress, "working") {
		t.Errorf("progress = %q, want a heartbeat", progress)
	}
}

func TestTokens(t *testing.T) {
	ts, journal := startServer(t, serveConfig{
		claude: fakeClaude(t, answeringClaude),
		tokens: map[string]string{"s3cret": "ana"},
	})

	t.Setenv("ASK_TOKEN", "wrong")
	if _, _, err := ask(t, ts.URL, "q"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: err = %v", err)
	}
	t.Setenv("ASK_TOKEN", "s3cret")
	if _, _, err := ask(t, ts.URL, "q"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(journal.String(), `"who":"ana"`) {
		t.Errorf("journal = %q, want the token's name", journal.String())
	}
}

func TestRejectsBadRequests(t *testing.T) {
	ts, _ := startServer(t, serveConfig{claude: fakeClaude(t, answeringClaude)})
	for name, body := range map[string]string{
		"empty question":   `{"question":"  "}`,
		"thread as a flag": `{"question":"q","thread":"--dangerously-skip-permissions"}`,
		"not json":         `nope`,
	} {
		resp, err := http.Post(ts.URL+"/ask", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8700": true,
		"localhost:8700": true,
		"[::1]:8700":     true,
		":8700":          false,
		"0.0.0.0:8700":   false,
		"10.0.0.5:8700":  false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

const attachingClaude = `cat >/dev/null
printf 'id,value\n1,BULKY-CONTENT\n' > "$ASK_OUTBOX/rows.csv"
ln -s /etc/hosts "$ASK_OUTBOX/link"
mkdir "$ASK_OUTBOX/dir"
echo '{"type":"result","subtype":"success","result":"see the attachment","session_id":"` + fakeSession + `"}'
`

func TestAttachmentsAreSavedAndKeptOutOfTheOutput(t *testing.T) {
	outbox := t.TempDir()
	ts, journal := startServer(t, serveConfig{claude: fakeClaude(t, attachingClaude), outbox: outbox})
	saved := t.TempDir()
	t.Setenv("ASK_ATTACHMENTS", saved)

	out, progress, err := ask(t, ts.URL, "list the rows")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(saved, fakeSession, "rows.csv")
	if want := "see the attachment\n\nattachments:\n  " + path + "  (25 B)\nthread: " + fakeSession + "\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if strings.Contains(out+progress, "BULKY-CONTENT") {
		t.Error("the attachment's content reached the output")
	}
	if got, _ := os.ReadFile(path); string(got) != "id,value\n1,BULKY-CONTENT\n" {
		t.Errorf("saved file = %q", got)
	}
	for _, name := range []string{"link", "dir"} {
		if !strings.Contains(progress, `attachment "`+name+`" left out`) {
			t.Errorf("progress = %q, want %q reported as left out", progress, name)
		}
	}
	if !strings.Contains(journal.String(), `"name":"rows.csv"`) {
		t.Errorf("journal = %q, want the attachment", journal.String())
	}

	// The same name in the same thread must not replace the first file.
	out, _, err = ask(t, ts.URL, "--thread", fakeSession, "again")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, filepath.Join(saved, fakeSession, "rows-2.csv")) {
		t.Errorf("second stdout = %q, want a numbered file", out)
	}
}

func TestRunWithoutAttachmentsLeavesNoOutbox(t *testing.T) {
	outbox := t.TempDir()
	ts, _ := startServer(t, serveConfig{claude: fakeClaude(t, answeringClaude), outbox: outbox})
	if _, _, err := ask(t, ts.URL, "q"); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(outbox); len(left) != 0 {
		t.Errorf("outbox holds %d entries, want none", len(left))
	}
}

func TestAttachmentRouteServesOnlyOutboxFiles(t *testing.T) {
	outbox := t.TempDir()
	run := strings.Repeat("ab", 16)
	if err := os.MkdirAll(filepath.Join(outbox, run), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(outbox, run, "ok.txt"), []byte("ok"), 0o600)
	os.WriteFile(filepath.Join(outbox, "outside.txt"), []byte("no"), 0o600)
	os.Symlink("/etc/hosts", filepath.Join(outbox, run, "link"))
	ts, _ := startServer(t, serveConfig{
		claude: fakeClaude(t, answeringClaude),
		outbox: outbox,
		tokens: map[string]string{"s3cret": "ana"},
	})

	get := func(path, token string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for path, want := range map[string]int{
		"/attachments/" + run + "/ok.txt":           http.StatusOK,
		"/attachments/" + run + "/link":             http.StatusNotFound,
		"/attachments/" + run + "/missing":          http.StatusNotFound,
		"/attachments/" + run + "/..%2Foutside.txt": http.StatusNotFound,
		"/attachments/" + run + "/%2E%2E":           http.StatusNotFound,
		"/attachments/not-a-run/ok.txt":             http.StatusNotFound,
	} {
		if got := get(path, "s3cret"); got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
	if got := get("/attachments/"+run+"/ok.txt", ""); got != http.StatusUnauthorized {
		t.Errorf("GET without a token = %d, want 401", got)
	}
}

// readingClaude answers with the content of the first file named in the
// question, the way the server lists files a caller sent.
const readingClaude = `q=$(cat)
f=$(printf '%s\n' "$q" | sed -n 's/^- //p' | head -1)
echo '{"type":"result","subtype":"success","result":"file says '"$(cat "$f")"'","session_id":"` + fakeSession + `"}'
`

func TestFilesSentWithAQuestionReachClaude(t *testing.T) {
	inbox := t.TempDir()
	ts, journal := startServer(t, serveConfig{claude: fakeClaude(t, readingClaude), inbox: inbox})
	file := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(file, []byte("hello-from-caller"), 0o600)

	out, _, err := ask(t, ts.URL, "--attach", file, "what", "is", "this")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "file says hello-from-caller\n") {
		t.Errorf("stdout = %q", out)
	}

	var entry journalEntry
	if err := json.Unmarshal(journal.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Question != "what is this" {
		t.Errorf("journal question = %q, want the caller's own words", entry.Question)
	}
	if len(entry.Attached) != 1 || entry.Attached[0].Name != "notes.txt" || entry.Attached[0].Size != 17 {
		t.Errorf("journal attached = %+v", entry.Attached)
	}
}

func TestRejectedUploadLeavesNoInbox(t *testing.T) {
	inbox := t.TempDir()
	ts, _ := startServer(t, serveConfig{claude: fakeClaude(t, readingClaude), inbox: inbox})
	dir := t.TempDir()
	same := filepath.Join(dir, "a.txt")
	os.WriteFile(same, []byte("x"), 0o600)

	_, _, err := ask(t, ts.URL, "--attach", same, "--attach", same, "q")
	if err == nil || !strings.Contains(err.Error(), "sent twice") {
		t.Errorf("err = %v, want the duplicate refused", err)
	}
	if left, _ := os.ReadDir(inbox); len(left) != 0 {
		t.Errorf("inbox holds %d entries, want none", len(left))
	}

	if _, _, err := ask(t, ts.URL, "--attach", filepath.Join(dir, "missing"), "q"); err == nil {
		t.Error("a missing file was accepted")
	}
}
