package main

// The wire protocol is one POST /ask. Its body is an askRequest as JSON, or
// multipart/form-data with the askRequest in the field "request" and one file
// per "attachment" part when the caller sends files along. The response is
// newline-delimited JSON: any number of progress events, then exactly one
// answer or error event. An answer can list attachments; each is fetched with
// GET on its Path.

type askRequest struct {
	Question string `json:"question"`
	// Thread continues an earlier conversation. Empty starts a new one.
	Thread string `json:"thread,omitempty"`
}

const (
	eventProgress = "progress"
	eventAnswer   = "answer"
	eventError    = "error"
)

type event struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Thread is the id to pass with a follow-up question. It can differ from
	// the one that was sent, so callers always use the latest.
	Thread      string       `json:"thread,omitempty"`
	CostUSD     float64      `json:"cost_usd,omitempty"`
	DurationMS  int64        `json:"duration_ms,omitempty"`
	Attachments []attachment `json:"attachments,omitempty"`
}

// attachment is a file that travels next to a question or an answer instead
// of inside its text. On an answer it holds output that is too large to be
// worth reading in full: the answer carries the summary and the file carries
// the data.
type attachment struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Path is the URL path of the file on the server. Empty for a file the
	// caller sent.
	Path string `json:"path,omitempty"`
}
