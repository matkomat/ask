package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type serveConfig struct {
	listen      string
	workspace   string
	claude      string
	claudeArgs  []string
	maxParallel int
	timeout     time.Duration
	heartbeat   time.Duration
	// tokens maps a bearer token to the name of the person who holds it.
	// Nil turns authentication off.
	tokens map[string]string
	// journal receives one JSON line per answered question. May be nil.
	journal io.Writer
	// outbox holds one directory per run. Claude gets its run's directory in
	// ASK_OUTBOX, and the files it leaves there become attachments.
	outbox string
	// inbox holds one directory per run for the files a caller sent with
	// the question.
	inbox         string
	attachmentTTL time.Duration
}

const (
	maxAttachments     = 10
	maxAttachmentBytes = 50 << 20
)

// server answers questions by running one headless Claude Code process per
// question in the workspace directory. A thread id is the Claude session id,
// so the server keeps no conversation state of its own: Claude Code stores the
// transcript and a follow-up resumes it.
type server struct {
	cfg serveConfig
	sem chan struct{}

	mu      sync.Mutex
	threads map[string]*sync.Mutex

	journalMu sync.Mutex
}

func newServer(cfg serveConfig) *server {
	return &server{
		cfg:     cfg,
		sem:     make(chan struct{}, cfg.maxParallel),
		threads: map[string]*sync.Mutex{},
	}
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, " ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func runServe(args []string) error {
	fs := flag.NewFlagSet("ask serve", flag.ExitOnError)
	cfg := serveConfig{heartbeat: 15 * time.Second}
	var extra stringList
	fs.StringVar(&cfg.listen, "listen", "127.0.0.1:8700", "address to listen on")
	fs.StringVar(&cfg.workspace, "workspace", ".", "directory Claude runs in")
	fs.StringVar(&cfg.claude, "claude", "claude", "Claude Code binary")
	fs.Var(&extra, "claude-arg", "extra argument for every Claude run (repeatable)")
	fs.IntVar(&cfg.maxParallel, "max-parallel", 3, "questions answered at the same time")
	fs.DurationVar(&cfg.timeout, "timeout", 15*time.Minute, "time limit for one answer")
	tokensPath := fs.String("tokens", "", `JSON file {"<token>": "<name>"}; required unless listening on loopback`)
	journalPath := fs.String("journal", "ask-journal.jsonl", "file that records every question and answer")
	fs.StringVar(&cfg.outbox, "outbox", filepath.Join(os.TempDir(), "ask-outbox"), "directory for attachments of answers")
	fs.StringVar(&cfg.inbox, "inbox", filepath.Join(os.TempDir(), "ask-inbox"), "directory for attachments of questions")
	fs.DurationVar(&cfg.attachmentTTL, "attachment-ttl", 24*time.Hour, "how long attachments are kept")
	fs.Parse(args)
	cfg.claudeArgs = extra

	if info, err := os.Stat(cfg.workspace); err != nil || !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", cfg.workspace)
	}
	if cfg.maxParallel < 1 {
		return errors.New("--max-parallel must be at least 1")
	}
	if *tokensPath != "" {
		b, err := os.ReadFile(*tokensPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &cfg.tokens); err != nil {
			return fmt.Errorf("%s: %w", *tokensPath, err)
		}
		if len(cfg.tokens) == 0 {
			return fmt.Errorf("%s holds no tokens", *tokensPath)
		}
	} else if !isLoopback(cfg.listen) {
		return errors.New("--tokens is required when --listen is not a loopback address")
	}
	if *journalPath != "" {
		f, err := os.OpenFile(*journalPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		cfg.journal = f
	}

	// Claude resolves paths against its own directory, so it needs both
	// attachment directories as absolute paths.
	for _, dir := range []*string{&cfg.outbox, &cfg.inbox} {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return err
		}
		*dir = abs
	}
	go func() {
		for {
			removeExpiredRuns(cfg.outbox, cfg.attachmentTTL)
			removeExpiredRuns(cfg.inbox, cfg.attachmentTTL)
			time.Sleep(time.Hour)
		}
	}()

	log.Printf("ask serve: listening on %s, workspace %s", cfg.listen, cfg.workspace)
	return http.ListenAndServe(cfg.listen, newServer(cfg).handler())
}

func removeExpiredRuns(root string, ttl time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && runPattern.MatchString(e.Name()) && time.Since(info.ModTime()) > ttl {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// A thread id ends up on Claude's command line, so only a session id may pass.
var threadPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

// A run id names the run's directories under the outbox and the inbox, and is
// part of attachment URLs.
var runPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ask", s.handleAsk)
	mux.HandleFunc("GET /attachments/{run}/{name}", s.handleAttachment)
	return mux
}

func (s *server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(r); !ok {
		http.Error(w, "missing or unknown token", http.StatusUnauthorized)
		return
	}
	run, name := r.PathValue("run"), r.PathValue("name")
	if !runPattern.MatchString(run) || !plainFileName(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.cfg.outbox, run, name)
	// Lstat, so that a link left in the outbox cannot serve a file from
	// elsewhere on the host.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// plainFileName reports whether name is a single path element that cannot
// point outside the directory it is joined to.
func plainFileName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && name == filepath.Base(name)
}

func (s *server) handleAsk(w http.ResponseWriter, r *http.Request) {
	who, ok := s.authenticate(r)
	if !ok {
		http.Error(w, "missing or unknown token", http.StatusUnauthorized)
		return
	}
	run := newRunID()
	req, attached, err := s.readRequest(w, r, run)
	if err == nil && strings.TrimSpace(req.Question) == "" {
		err = errors.New("empty question")
	}
	if err == nil && req.Thread != "" && !threadPattern.MatchString(req.Thread) {
		err = errors.New("malformed thread id")
	}
	if err != nil {
		os.RemoveAll(filepath.Join(s.cfg.inbox, run))
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	send := func(ev event) {
		// A failed write means the caller left. The request context is then
		// cancelled, which stops Claude; the loop below still has to drain.
		_ = enc.Encode(ev)
		_ = rc.Flush()
	}

	events := make(chan event, 16)
	go func() {
		defer close(events)
		s.answer(r.Context(), who, run, req, attached, events)
	}()

	// The heartbeat keeps proxies and the caller from treating a long stretch
	// of thinking as a dead connection.
	beat := time.NewTicker(s.cfg.heartbeat)
	defer beat.Stop()
	for {
		select {
		case ev, open := <-events:
			if !open {
				return
			}
			send(ev)
		case <-beat.C:
			send(event{Type: eventProgress, Text: "working"})
		}
	}
}

// readRequest reads the question and stores the files sent with it in the
// run's inbox directory. On an error the caller removes that directory.
func (s *server) readRequest(w http.ResponseWriter, r *http.Request, run string) (askRequest, []attachment, error) {
	var req askRequest
	kind, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "multipart/form-data" {
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		return req, nil, err
	}

	dir := filepath.Join(s.cfg.inbox, run)
	var files []attachment
	mr := multipart.NewReader(http.MaxBytesReader(w, r.Body, maxAttachments*maxAttachmentBytes+1<<20), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return req, files, nil
		}
		if err != nil {
			return req, nil, err
		}
		switch part.FormName() {
		case "request":
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&req); err != nil {
				return req, nil, err
			}
		case "attachment":
			name := part.FileName()
			if !plainFileName(name) {
				return req, nil, fmt.Errorf("unusable attachment name %q", name)
			}
			if len(files) == maxAttachments {
				return req, nil, fmt.Errorf("more than %d attachments", maxAttachments)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return req, nil, err
			}
			a, err := storeAttachment(filepath.Join(dir, name), part)
			if err != nil {
				return req, nil, fmt.Errorf("attachment %q: %w", name, err)
			}
			files = append(files, a)
		}
	}
}

func storeAttachment(path string, r io.Reader) (attachment, error) {
	// O_EXCL, so that two parts with one name are an error and not a
	// silent replacement.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return attachment{}, errors.New("sent twice")
	}
	if err != nil {
		return attachment{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxAttachmentBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return attachment{}, err
	}
	if n > maxAttachmentBytes {
		return attachment{}, fmt.Errorf("larger than %d MB", maxAttachmentBytes>>20)
	}
	return attachment{Name: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func (s *server) authenticate(r *http.Request) (string, bool) {
	if s.cfg.tokens == nil {
		return "anonymous", true
	}
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		return "", false
	}
	who, ok := s.cfg.tokens[token]
	return who, ok
}

func (s *server) threadLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.threads[id]
	if !ok {
		l = &sync.Mutex{}
		s.threads[id] = l
	}
	return l
}

// answer runs Claude for one question and sends progress events followed by
// exactly one answer or error event. attached are the files already stored in
// the run's inbox directory.
func (s *server) answer(ctx context.Context, who, run string, req askRequest, attached []attachment, out chan<- event) {
	start := time.Now()
	final := event{Type: eventError, Thread: req.Thread}
	defer func() {
		final.DurationMS = time.Since(start).Milliseconds()
		// The journal is written first, so a caller that has its answer can
		// rely on the entry being there.
		s.record(who, req, attached, final)
		out <- final
	}()

	// Two runs that resume the same session would write the same transcript.
	if req.Thread != "" {
		l := s.threadLock(req.Thread)
		l.Lock()
		defer l.Unlock()
	}

	select {
	case s.sem <- struct{}{}:
	default:
		out <- event{Type: eventProgress, Text: "queued behind other questions"}
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			final.Text = "cancelled while queued"
			return
		}
	}
	defer func() { <-s.sem }()

	ctx, cancel := context.WithTimeout(ctx, s.cfg.timeout)
	defer cancel()

	// stream-json prints one event per line while Claude works; print mode
	// only emits it together with --verbose.
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if req.Thread != "" {
		args = append(args, "--resume", req.Thread)
	}

	outbox := filepath.Join(s.cfg.outbox, run)
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		final.Text = "cannot prepare the outbox: " + err.Error()
		return
	}
	defer func() {
		if len(final.Attachments) == 0 {
			os.RemoveAll(outbox)
		}
	}()
	// --add-dir puts the outbox among Claude's working directories. Whether
	// Claude may write there is still up to the workspace's permission rules.
	args = append(args, "--add-dir", outbox)
	// The path is also given in words: permission rules can keep Claude from
	// reading its environment, and then ASK_OUTBOX alone would not reach it.
	args = append(args, "--append-system-prompt",
		"Attachment directory for this question (also in the environment variable ASK_OUTBOX): "+outbox)
	// The files a caller sent are named in the question itself, not in the
	// system prompt: their names come from the caller, like the question.
	prompt := req.Question
	if len(attached) > 0 {
		inbox := filepath.Join(s.cfg.inbox, run)
		args = append(args, "--add-dir", inbox)
		prompt += "\n\nFiles attached to this question:"
		for _, a := range attached {
			prompt += "\n- " + filepath.Join(inbox, a.Name)
		}
	}
	args = append(args, s.cfg.claudeArgs...)

	cmd := exec.CommandContext(ctx, s.cfg.claude, args...)
	cmd.Dir = s.cfg.workspace
	cmd.Env = append(os.Environ(), "ASK_OUTBOX="+outbox)
	// The question travels on stdin so that no text in it can act as a flag.
	cmd.Stdin = strings.NewReader(prompt)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		final.Text = err.Error()
		return
	}
	if err := cmd.Start(); err != nil {
		final.Text = "cannot start Claude: " + err.Error()
		return
	}

	var result *claudeEvent
	lastProgress := ""
	r := bufio.NewReader(stdout)
	for {
		line, readErr := r.ReadBytes('\n')
		if ev, ok := parseClaudeEvent(line); ok {
			if ev.SessionID != "" {
				final.Thread = ev.SessionID
			}
			switch ev.Type {
			case "assistant":
				for _, tool := range ev.toolNames() {
					// Only the tool name leaves the host: tool input can
					// hold queries and paths the caller should not see.
					if text := "using " + tool; text != lastProgress {
						lastProgress = text
						out <- event{Type: eventProgress, Text: text}
					}
				}
			case "result":
				result = &ev
			}
		}
		if readErr != nil {
			break
		}
	}
	waitErr := cmd.Wait()

	switch {
	case result != nil && !result.IsError:
		final.Type = eventAnswer
		final.Text = result.Result
		final.CostUSD = result.TotalCostUSD
		final.Attachments = collectAttachments(run, outbox, out)
	case result != nil:
		final.CostUSD = result.TotalCostUSD
		// A failed run can still carry the subtype "success", for example
		// when the API refuses the credentials; then only the text says why.
		final.Text = "Claude failed"
		if result.Subtype != "" && result.Subtype != "success" {
			final.Text += " (" + result.Subtype + ")"
		}
		if result.Result != "" {
			final.Text += ": " + result.Result
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		final.Text = fmt.Sprintf("no answer within %s", s.cfg.timeout)
	case ctx.Err() != nil:
		final.Text = "cancelled"
	default:
		final.Text = fmt.Sprintf("Claude ended without an answer (%v)", waitErr)
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			final.Text += ": " + tail
		}
	}
}

func newRunID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// collectAttachments lists the regular files at the top of a run's outbox.
// Anything else there is left out and reported as a progress event.
func collectAttachments(run, dir string, out chan<- event) []attachment {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var list []attachment
	skip := func(name, why string) {
		out <- event{Type: eventProgress, Text: fmt.Sprintf("attachment %q left out: %s", name, why)}
	}
	for _, e := range entries {
		info, err := e.Info()
		switch {
		case err != nil || !info.Mode().IsRegular():
			skip(e.Name(), "not a regular file")
			continue
		case !plainFileName(e.Name()):
			skip(e.Name(), "unusable file name")
			continue
		case info.Size() > maxAttachmentBytes:
			skip(e.Name(), fmt.Sprintf("larger than %d MB", maxAttachmentBytes>>20))
			continue
		case len(list) == maxAttachments:
			skip(e.Name(), fmt.Sprintf("more than %d attachments", maxAttachments))
			continue
		}
		sum, err := fileSHA256(filepath.Join(dir, e.Name()))
		if err != nil {
			skip(e.Name(), "unreadable")
			continue
		}
		list = append(list, attachment{
			Name:   e.Name(),
			Size:   info.Size(),
			SHA256: sum,
			Path:   "/attachments/" + run + "/" + url.PathEscape(e.Name()),
		})
	}
	return list
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// claudeEvent is the part of a Claude Code stream-json line that the server
// reads.
type claudeEvent struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	SessionID    string  `json:"session_id"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Message      struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

func parseClaudeEvent(line []byte) (claudeEvent, bool) {
	var ev claudeEvent
	line = bytes.TrimSpace(line)
	if len(line) == 0 || json.Unmarshal(line, &ev) != nil {
		return ev, false
	}
	return ev, true
}

func (e claudeEvent) toolNames() []string {
	var names []string
	for _, c := range e.Message.Content {
		if c.Type == "tool_use" && c.Name != "" {
			names = append(names, c.Name)
		}
	}
	return names
}

type journalEntry struct {
	Time       time.Time `json:"time"`
	Who        string    `json:"who"`
	Thread     string    `json:"thread,omitempty"`
	FollowUp   bool      `json:"follow_up"`
	Question   string    `json:"question"`
	Outcome    string    `json:"outcome"`
	Answer     string    `json:"answer"`
	CostUSD    float64   `json:"cost_usd"`
	DurationMS int64     `json:"duration_ms"`
	// Attachments travel outside the question and answer text, so the
	// journal names each one and its hash: Attached came with the question,
	// Attachments went out with the answer.
	Attached    []attachment `json:"attached,omitempty"`
	Attachments []attachment `json:"attachments,omitempty"`
}

func (s *server) record(who string, req askRequest, attached []attachment, final event) {
	log.Printf("ask: who=%s thread=%s outcome=%s duration=%dms cost=$%.4f",
		who, final.Thread, final.Type, final.DurationMS, final.CostUSD)
	if s.cfg.journal == nil {
		return
	}
	line, err := json.Marshal(journalEntry{
		Time:        time.Now().UTC(),
		Who:         who,
		Thread:      final.Thread,
		FollowUp:    req.Thread != "",
		Question:    req.Question,
		Outcome:     final.Type,
		Answer:      final.Text,
		CostUSD:     final.CostUSD,
		DurationMS:  final.DurationMS,
		Attached:    attached,
		Attachments: final.Attachments,
	})
	if err != nil {
		return
	}
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	if _, err := s.cfg.journal.Write(append(line, '\n')); err != nil {
		log.Printf("ask: journal write failed: %v", err)
	}
}

// tailBuffer keeps the last bytes written to it, enough to show why a process
// failed without holding everything it printed.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailLimit = 2048

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailLimit {
		t.buf = t.buf[len(t.buf)-tailLimit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
