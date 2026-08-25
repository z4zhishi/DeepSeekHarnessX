package llm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
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
//
// The timeout arms a connect+headers deadline only: consumeSSE detaches the
// stream from it once response headers arrive, so a healthy SSE body may
// outlive `timeout` and is bounded solely by the idle watchdog (the contract's
// "watchdog uses per-line read timeouts, not a global deadline"). Parent
// cancellation still propagates through streamCtx for the whole call.
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

// ErrDeepSeekMalformed marks a malformed SSE data payload: a payload that
// arrived well-framed but failed JSON decoding in the adapter's translate
// step. Deliberately distinct from ErrDeepSeekStream (truncation semantics,
// classified retryable TRANSPORT by pkg/agent/loop.go): a malformed payload is
// a provider-side protocol fault, so this sentinel intentionally falls outside
// the retryable set.
var ErrDeepSeekMalformed = errors.New("deepseek: malformed SSE data payload")

// malformedPayloadPreviewMax caps the offending payload echoed in the error
// text, mirroring upstream translate.ts (MALFORMED_RESPONSE carries the first
// 120 characters).
const malformedPayloadPreviewMax = 120

// errMalformedPayload builds the ErrDeepSeekMalformed-wrapped error for a
// payload that failed JSON decoding, embedding at most the first 120 runes of
// the offending load for diagnosis.
func errMalformedPayload(payload string) error {
	preview := payload
	if utf8.RuneCountInString(preview) > malformedPayloadPreviewMax {
		runes := []rune(preview)
		preview = string(runes[:malformedPayloadPreviewMax])
	}
	return fmt.Errorf("%w: %s", ErrDeepSeekMalformed, preview)
}

// resolveStreamKey resolves an explicit API key or falls back to the profile's
// credential resolver.
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

// sseBOM is the UTF-8 byte order mark; some gateways prepend it to the SSE
// byte stream and it must never leak into the first field value.
const sseBOM = "\xEF\xBB\xBF"

// sseEventBuffer buffers one SSE event at a time and dispatches strictly on
// the event's blank-line terminator (docs/deepseek-llm-contract.md "SSE 解析",
// mirroring upstream CK/packages/llm/llm-deepseek/src/sse.ts, whose framing is
// eventsource-parser's):
//
//   - multiple `data:` lines of the same event accumulate and are joined with
//     "\n" at the terminator — dispatching each line independently would hand
//     the caller the first fragment of a split JSON payload;
//   - comment lines (`:` prefix) and non-data fields (event:/id:/retry:) are
//     skipped and never reach the payload stream;
//   - the UTF-8 BOM is stripped from the first line of the stream;
//   - an event still unterminated at EOF is dropped, never flushed
//     (spec-strict truncation, see consumeSSE).
//
// Lines arrive CRLF-pretrimmed (bufio.Scanner strips the trailing \r), so the
// blank-line terminator matches both "\n" and "\r\n" framings.
type sseEventBuffer struct {
	data  []string // buffered `data:` values of the in-flight event
	first bool     // next line is the stream's first line (BOM window)
}

func newSSEEventBuffer() *sseEventBuffer {
	return &sseEventBuffer{first: true}
}

// accept feeds one physical line and returns (payload, true) exactly when the
// line terminated an event that carried data — the only dispatch point.
func (b *sseEventBuffer) accept(line string) (string, bool) {
	if b.first {
		b.first = false
		line = strings.TrimPrefix(line, sseBOM)
	}
	switch {
	case line == "":
		// Blank line: the event terminator and sole dispatch point.
		if len(b.data) == 0 {
			return "", false
		}
		payload := strings.Join(b.data, "\n")
		b.data = b.data[:0]
		return payload, true
	case strings.HasPrefix(line, ":"):
		// Comment: transport activity only, never enters the payload stream.
		return "", false
	case strings.HasPrefix(line, "data:"):
		// Field value: strip the prefix and trim surrounding whitespace
		// (spec strips one leading space; trimming is the established
		// superset here and JSON/[DONE] payloads carry no meaningful edges).
		if v := strings.TrimSpace(strings.TrimPrefix(line, "data:")); v != "" {
			b.data = append(b.data, v)
		}
		return "", false
	default:
		// Any other field line (event:, id:, retry:, unknown) is skipped.
		return "", false
	}
}

// pending reports whether an event is buffered but not yet terminated. Such
// an event is discarded at EOF — it is truncation evidence, not a flushable
// payload.
func (b *sseEventBuffer) pending() bool {
	return len(b.data) > 0
}

// pendingJoined renders the unterminated event's data as it would have been
// dispatched (diagnostics only; the event itself is dropped).
func (b *sseEventBuffer) pendingJoined() string {
	return strings.Join(b.data, "\n")
}

// consumeSSE POSTs (the request is already built) and drives the idle
// watchdog over SSE events. handle returns done=true on a terminal payload
// ([DONE]). Events dispatch only on their blank-line terminator, with the
// event's data lines joined by "\n".
//
// Error surface:
//   - parent cancel/connect-timeout surfaces as an error (never a clean EOF);
//   - idle watchdog fires ErrDeepSeekWatchdog;
//   - clean EOF without a dispatched [DONE], or with a still-unterminated
//     trailing event (even one whose data equals "[DONE]") is ErrDeepSeekStream
//     — STREAM_CLOSED truncation semantics, matching upstream sse.ts.
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

	// Detach the stream from the connect+headers timeout: once headers are in,
	// the body is bounded solely by the idle watchdog (per-line read timeouts),
	// never by a global wall clock. Parent cancellation still propagates.
	bodyCtx, bodyCancel := context.WithCancel(context.WithoutCancel(streamCtx))
	defer bodyCancel()
	go readSSELines(bodyCtx, resp.Body, lineCh, readErr)

	timer := time.NewTimer(watchdog)
	defer timer.Stop()

	events := newSSEEventBuffer()
	for {
		select {
		case <-streamCtx.Done():
			// Parent cancel/timeout (the deadline only covers connect+headers):
			// surface as an error, never as a clean EOF — a silent nil return
			// would persist a partial response as success.
			return streamCtx.Err()
		case err := <-readErr:
			if err == nil {
				// readSSELines signals cancellation of the detached body reader
				// by closing readErr after consumeSSE returned; unreachable in
				// practice because this loop has already exited.
				err = ErrDeepSeekStream
			}
			return err
		case <-timer.C:
			cancel()
			return ErrDeepSeekWatchdog
		case line, ok := <-lineCh:
			if !ok {
				// EOF: enforce truncation discipline (drops any unterminated
				// trailing event, [DONE]-looking or not) before declaring the
				// missing-[DONE] truncation.
				return sseEOFPending(events)
			}
			timer.Reset(watchdog)
			payload, dispatch := events.accept(line)
			if !dispatch {
				continue
			}
			done, err := handle(payload)
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

// sseEOFPending classifies EOF for the framing state machine: an event that
// never saw its blank-line terminator is discarded — never flushed — even when
// its buffered data happens to be "[DONE]" (upstream treats an unterminated
// tail as STREAM_CLOSED; silently accepting it would mask a truncated
// response as a clean completion). A fully drained stream without a
// dispatched [DONE] is likewise truncation.
func sseEOFPending(events *sseEventBuffer) error {
	if events.pending() {
		// Cap the diagnostic echo so a pathological tail can't flood the log.
		return fmt.Errorf("%w: EOF inside unterminated SSE event (discarded): %.120s",
			ErrDeepSeekStream, events.pendingJoined())
	}
	return ErrDeepSeekStream
}

// readSSELines reads lines from the SSE body and pushes them onto lineCh.
// It blocks until the stream is exhausted or ctx is canceled; a read failure
// is reported on readErr. On clean EOF it closes lineCh (the canonical EOF
// marker); on cancellation it closes both channels so no receiver blocks.
func readSSELines(ctx context.Context, r io.Reader, lineCh chan<- string, readErr chan<- error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		select {
		case lineCh <- sc.Text():
		case <-ctx.Done():
			close(lineCh)
			close(readErr)
			return
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		select {
		case readErr <- err:
		case <-ctx.Done():
		}
		close(lineCh)
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
