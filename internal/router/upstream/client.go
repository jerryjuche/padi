// Package upstream performs the actual OpenAI-compatible provider call.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jerryjuche/padi/internal/router/config"
	"github.com/jerryjuche/padi/internal/router/route"
)

// Result is the outcome of one provider attempt.
type Result struct {
	Outcome    Outcome
	Status     int
	RetryAfter time.Duration
	FirstByte  time.Duration
	TokensIn   int
	TokensOut  int
	MidStream  bool
}

// Client calls OpenAI-compatible providers without writing to the response
// until the first upstream byte arrives, so failures remain fail-overable.
type Client struct {
	HTTP             *http.Client
	FirstByteTimeout time.Duration
	TotalTimeout     time.Duration
}

func New(firstByte, total time.Duration) *Client {
	return &Client{
		HTTP:             newHTTPClient(firstByte),
		FirstByteTimeout: firstByte,
		TotalTimeout:     total,
	}
}

func newHTTPClient(firstByte time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = firstByte
	return &http.Client{Transport: transport}
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Do rewrites only the JSON model field and forwards router headers.
func (c *Client) Do(ctx context.Context, t config.Target, p config.Provider, req route.Request, w http.ResponseWriter) (Result, error) {
	start := time.Now()
	if c.TotalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.TotalTimeout)
		defer cancel()
	}

	var payload map[string]any
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		return Result{Outcome: BadRequest, Status: http.StatusBadRequest}, fmt.Errorf("upstream: invalid request body: %w", err)
	}
	payload["model"] = t.Model
	if req.Stream && p.SupportsStream {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{Outcome: BadRequest, Status: http.StatusBadRequest}, err
	}

	endpoint := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{Outcome: Retryable}, err
	}
	upReq.Header.Set("Content-Type", "application/json")
	for k, v := range req.Headers {
		if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "content-length") || strings.EqualFold(k, "host") {
			continue
		}
		upReq.Header.Set(k, v)
	}
	if p.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = newHTTPClient(c.FirstByteTimeout)
	}
	resp, err := httpClient.Do(upReq)
	if err != nil {
		return Result{Outcome: Retryable}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return Result{
			Outcome:    Classify(resp.StatusCode, resp.Header, raw, nil),
			Status:     resp.StatusCode,
			RetryAfter: RetryAfter(resp.Header),
		}, nil
	}

	copyHeaders(w.Header(), resp.Header)
	if req.Stream {
		w.Header().Del("Content-Length")
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
		}
	}

	reader := bufio.NewReader(resp.Body)
	if _, err := reader.Peek(1); err != nil {
		return Result{Outcome: Retryable, Status: resp.StatusCode}, err
	}
	res := Result{Outcome: OK, Status: resp.StatusCode, FirstByte: time.Since(start)}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	if !req.Stream {
		raw, err := io.ReadAll(reader)
		if err != nil {
			writeSSEError(w, flusher, err.Error())
			res.Outcome, res.MidStream = Retryable, true
			return res, nil
		}
		var decoded struct {
			Usage usage `json:"usage"`
		}
		if json.Unmarshal(raw, &decoded) == nil {
			res.TokensIn, res.TokensOut = decoded.Usage.PromptTokens, decoded.Usage.CompletionTokens
		}
		_, _ = w.Write(raw)
		if flusher != nil {
			flusher.Flush()
		}
		return res, nil
	}

	buf := make([]byte, 8192)
	seenDone := false
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
			if bytes.Contains(bytes.ToLower(chunk), []byte(`"error"`)) {
				res.Outcome, res.MidStream = Retryable, true
			}
			if bytes.Contains(chunk, []byte("[DONE]")) {
				seenDone = true
			}
			if u, ok := parseUsage(chunk); ok {
				res.TokensIn, res.TokensOut = u.PromptTokens, u.CompletionTokens
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			writeSSEError(w, flusher, readErr.Error())
			res.Outcome, res.MidStream = Retryable, true
			break
		}
	}
	if res.MidStream && !seenDone {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	return res, nil
}

func copyHeaders(dst, src http.Header) {
	for k, values := range src {
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func parseUsage(chunk []byte) (usage, bool) {
	var u usage
	line := strings.TrimSpace(string(chunk))
	line = strings.TrimPrefix(line, "data:")
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return u, false
	}
	var envelope struct {
		Usage usage `json:"usage"`
	}
	if json.Unmarshal([]byte(line), &envelope) != nil {
		return u, false
	}
	return envelope.Usage, true
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, message string) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "router_error"},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
