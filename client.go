package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const askUsage = `usage: ask [--thread ID] [--attach FILE]... [--server URL] <question>
       ask [--thread ID] [--attach FILE]... [--server URL] -    (question from stdin)
       ask serve [flags]

environment: ASK_SERVER (default http://127.0.0.1:8700), ASK_TOKEN,
             ASK_ATTACHMENTS (directory for attachments, default the user cache)
`

func runAsk(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, askUsage) }
	server := os.Getenv("ASK_SERVER")
	if server == "" {
		server = "http://127.0.0.1:8700"
	}
	fs.StringVar(&server, "server", server, "address of ask serve")
	thread := fs.String("thread", "", "continue the conversation with this thread id")
	attachDir := fs.String("attachments", os.Getenv("ASK_ATTACHMENTS"), "directory for attachments of the answer")
	var attach stringList
	fs.Var(&attach, "attach", "file to send with the question (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	question := strings.Join(fs.Args(), " ")
	if question == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		question = string(b)
	}
	if strings.TrimSpace(question) == "" {
		fs.Usage()
		return errors.New("no question given")
	}

	body, contentType, err := requestBody(askRequest{Question: question, Thread: *thread}, attach)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+"/ask", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	token := os.Getenv("ASK_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// No client timeout: an answer can take many minutes, and the server
	// keeps the connection busy with progress events.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("server answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	r := bufio.NewReader(resp.Body)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var ev event
			if err := json.Unmarshal(line, &ev); err != nil {
				return fmt.Errorf("unreadable event from server: %w", err)
			}
			switch ev.Type {
			case eventProgress:
				fmt.Fprintln(stderr, "…", ev.Text)
			case eventAnswer:
				// Only the path of an attachment is printed. The caller is
				// usually an agent, and what it reads from stdout fills its
				// context; the file is there for when it needs the data.
				fmt.Fprintln(stdout, ev.Text)
				fmt.Fprintln(stdout)
				var failed []string
				if len(ev.Attachments) > 0 {
					fmt.Fprintln(stdout, "attachments:")
				}
				for _, a := range ev.Attachments {
					path, err := download(server, token, *attachDir, ev.Thread, a)
					if err != nil {
						fmt.Fprintf(stdout, "  %s  (not downloaded: %v)\n", a.Name, err)
						failed = append(failed, a.Name)
						continue
					}
					fmt.Fprintf(stdout, "  %s  (%s)\n", path, humanSize(a.Size))
				}
				fmt.Fprintf(stdout, "thread: %s\n", ev.Thread)
				if len(failed) > 0 {
					return fmt.Errorf("could not download %s", strings.Join(failed, ", "))
				}
				return nil
			case eventError:
				if ev.Thread != "" {
					fmt.Fprintf(stdout, "thread: %s\n", ev.Thread)
				}
				return errors.New(ev.Text)
			}
		}
		if readErr != nil {
			return errors.New("the server closed the connection before it answered")
		}
	}
}

// download fetches one attachment into <dir>/<thread>/ and returns where it
// is. A file that is already there under the same name is kept: the new one
// gets a numbered name.
func download(server, token, dir, thread string, a attachment) (string, error) {
	name := filepath.Base(a.Name)
	if name != a.Name || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", errors.New("unusable file name")
	}
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "ask")
	}
	dir, err := filepath.Abs(filepath.Join(dir, filepath.Base(thread)))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(server, "/")+a.Path, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server answered %s", resp.Status)
	}

	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return "", errors.New("checksum mismatch")
	}

	ext := filepath.Ext(name)
	path := filepath.Join(dir, name)
	for n := 2; ; n++ {
		// Link fails when the name is taken, so two downloads cannot
		// replace each other's file.
		err := os.Link(tmp.Name(), path)
		if err == nil {
			return path, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), n, ext))
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// requestBody encodes a question. Without files it is plain JSON; with files
// it is a multipart form that is written while the request is being sent, so
// a large file is never held in memory.
func requestBody(req askRequest, files []string) (io.Reader, string, error) {
	if len(files) == 0 {
		b, err := json.Marshal(req)
		return bytes.NewReader(b), "application/json", err
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return nil, "", err
		}
		if !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("%s is not a regular file", path)
		}
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(func() error {
			field, err := mw.CreateFormField("request")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(field).Encode(req); err != nil {
				return err
			}
			for _, path := range files {
				part, err := mw.CreateFormFile("attachment", filepath.Base(path))
				if err != nil {
					return err
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.Copy(part, f)
				f.Close()
				if err != nil {
					return err
				}
			}
			return mw.Close()
		}())
	}()
	return pr, mw.FormDataContentType(), nil
}
