package llm

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// failStream returns already-closed stream channels carrying a single error.
// Matches the DeepSeekAdapter missing-credential path: the error is buffered
// before both channels close so drain helpers observe it without racing.
func failStream(err error) (<-chan StreamChunk, <-chan error) {
	chunkChan := make(chan StreamChunk, 64)
	errChan := make(chan error, 1)
	errChan <- err
	close(chunkChan)
	close(errChan)
	return chunkChan, errChan
}

// startStream runs fn under the adapter timeout. fn's returned error (if any)
// is the single fatal error delivered on the error channel.
func startStream(
	ctx context.Context,
	timeout time.Duration,
	fn func(streamCtx context.Context, cancel context.CancelFunc, chunks chan<- StreamChunk) error,
) (<-chan StreamChunk, <-chan error) {
	chunkChan := make(chan StreamChunk, 64)
	errChan := make(chan error, 1)
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	go func() {
		defer close(chunkChan)
		defer close(errChan)
		defer cancel()
		if err := fn(streamCtx, cancel, chunkChan); err != nil {
			errChan <- err
		}
	}()
	return chunkChan, errChan
}

func resolveStreamKey(apiKey string, resolver func() (string, error)) (string, error) {
	if apiKey != "" {
		return apiKey, nil
	}
	if resolver != nil {
		key, err := resolver()
		if err != nil {
			return "", err
		}
		if key != "" {
			return key, nil
		}
	}
	return "", ErrDeepSeekMissingCredential
}

func applyExtraHeaders(h http.Header, extra map[string]string) {
	for k, v := range extra {
		if k == "" || v == "" {
			continue
		}
		h.Set(k, v)
	}
}

func emitChunk(ctx context.Context, chunks chan<- StreamChunk, c StreamChunk) {
	select {
	case chunks <- c:
	case <-ctx.Done():
	}
}

func sseDataPayload(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" {
		return "", false
	}
	return data, true
}

// consumeSSE POSTs (the request is already built) and drives the idle
// watchdog over SSE lines. handle returns done=true on a terminal payload.
// A silent return on parent cancel matches the existing DeepSeek adapter.
func consumeSSE(
	streamCtx context.Context,
	cancel context.CancelFunc,
	httpc *http.Client,
	watchdog time.Duration,
	httpReq *http.Request,
	handle func(data string) (done bool, err error),
) error {
	resp, err := httpc.Do(httpReq)
	if err != nil {
		if streamCtx.Err() != nil {
			return streamCtx.Err()
		}
		return err
	}
	defer resp.Body.Close()

	if !isSuccessStatus(resp.StatusCode) {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return mapDeepSeekStatus(
			resp.StatusCode,
			parseWireError(raw, nil),
			resp.Header.Get("Retry-After"),
			requestIDOf(resp),
		)
	}

	lineCh := make(chan string, 64)
	readErr := make(chan error, 1)
	go readSSELines(streamCtx, resp.Body, lineCh, readErr)

	timer := time.NewTimer(watchdog)
	defer timer.Stop()

	for {
		select {
		case <-streamCtx.Done():
			return nil
		case err := <-readErr:
			return err
		case <-timer.C:
			cancel()
			return ErrDeepSeekWatchdog
		case line, ok := <-lineCh:
			if !ok {
				return ErrDeepSeekStream
			}
			timer.Reset(watchdog)
			data, ok := sseDataPayload(line)
			if !ok {
				continue
			}
			done, err := handle(data)
			if err != nil {
				cancel()
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// readSSELines reads lines from the SSE body and pushes them onto lineCh.
// It blocks until the stream is exhausted or streamCtx is canceled; a read
// failure is reported on readErr (nil is not sent on clean EOF).
func readSSELines(ctx context.Context, r io.Reader, lineCh chan<- string, readErr chan<- error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		select {
		case lineCh <- sc.Text():
		case <-ctx.Done():
			return
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		select {
		case readErr <- err:
		case <-ctx.Done():
		}
		return
	}
	// Clean EOF: signal the end of the byte stream so the caller can detect a
	// missing [DONE] sentinel (truncated response). Close, not nil-send: a
	// receive on a closed channel is the canonical EOF marker here.
	close(lineCh)
}

func trimBaseURL(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func pathHas(base, fragment string) bool {
	return strings.Contains(strings.ToLower(base), strings.ToLower(fragment))
}

func pathEnds(base, suffix string) bool {
	return strings.HasSuffix(strings.ToLower(base), strings.ToLower(suffix))
}
