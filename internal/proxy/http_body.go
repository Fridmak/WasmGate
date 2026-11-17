package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"wasmgate/internal/config"
	"wasmgate/internal/plugin/abi4"
)

var (
	errBodyFraming  = errors.New("proxy: unsupported or invalid HTTP/1 body framing")
	errBodyMutation = errors.New("proxy: body mutation changed chunk length")
)

// httpBodyInspector implements the deliberately narrow M5 framing contract:
// Content-Length is supported; Transfer-Encoding/chunked and ambiguous
// framing fail closed. It receives raw relay chunks and invokes typed v4 body
// phases without changing the splice decision for listeners without a policy.
type httpBodyInspector struct {
	engine        *abi4.Engine
	connID        uint64
	request       bool
	response      bool
	requestLeft   int64
	responseLeft  int64
	maxHeader     int
	respHeader    []byte
	respDone      bool
	requestState  *abi4.StateSlot
	responseState *abi4.StateSlot
}

func newHTTPBodyInspector(engine *abi4.Engine, connID uint64, headers []byte, policy string, maxHeader int) (*httpBodyInspector, error) {
	i := &httpBodyInspector{
		engine:    engine,
		connID:    connID,
		request:   policy == config.BodyPolicyRequest || policy == config.BodyPolicyBoth,
		response:  policy == config.BodyPolicyResponse || policy == config.BodyPolicyBoth,
		maxHeader: maxHeader,
	}
	if i.request {
		length, err := contentLength(headers)
		if err != nil {
			return nil, err
		}
		i.requestLeft = length
		i.requestState, err = abi4.NewStateSlot(4 * 1024)
		if err != nil {
			return nil, err
		}
	}
	if i.response {
		var err error
		i.responseState, err = abi4.NewStateSlot(4 * 1024)
		if err != nil {
			return nil, err
		}
	}
	return i, nil
}

func contentLength(block []byte) (int64, error) {
	lines := bytes.Split(block, []byte("\r\n"))
	var found *int64
	for _, line := range lines[1:] {
		if len(line) == 0 {
			break
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return 0, errBodyFraming
		}
		name := bytes.ToLower(bytes.TrimSpace(line[:colon]))
		value := bytes.TrimSpace(line[colon+1:])
		switch string(name) {
		case "transfer-encoding":
			return 0, fmt.Errorf("%w: transfer-encoding is not enabled in M5", errBodyFraming)
		case "content-length":
			if found != nil {
				return 0, fmt.Errorf("%w: duplicate content-length", errBodyFraming)
			}
			n, err := strconv.ParseInt(string(value), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("%w: invalid content-length", errBodyFraming)
			}
			found = &n
		}
	}
	if found == nil {
		return 0, nil
	}
	return *found, nil
}

func (i *httpBodyInspector) inspectRequestPrefix(ctx context.Context, prefix, headers []byte) (int32, error) {
	if !i.request || len(prefix) <= len(headers) {
		return 0, nil
	}
	return i.inspectRequest(ctx, prefix[len(headers):])
}

func (i *httpBodyInspector) inspectRequest(ctx context.Context, chunk []byte) (int32, error) {
	if !i.request || i.requestLeft <= 0 || len(chunk) == 0 {
		return 0, nil
	}
	n := len(chunk)
	if int64(n) > i.requestLeft {
		n = int(i.requestLeft)
	}
	result, err := i.engine.DispatchWithState(ctx, i.connID, abi4.PhaseRequestBody, 0,
		abi4.Event{Block: chunk[:n]}, i.requestState)
	if err != nil {
		return 1, err
	}
	if err := applyMutation(chunk[:n], result); err != nil {
		return 1, err
	}
	i.requestLeft -= int64(n)
	return int32(result.Verdict), nil
}

func (i *httpBodyInspector) inspectResponse(ctx context.Context, chunk []byte) (int32, error) {
	if !i.response || len(chunk) == 0 {
		return 0, nil
	}
	if !i.respDone {
		i.respHeader = append(i.respHeader, chunk...)
		if len(i.respHeader) > i.maxHeader {
			return 1, fmt.Errorf("%w: response headers exceed limit", errBodyFraming)
		}
		idx := bytes.Index(i.respHeader, []byte("\r\n\r\n"))
		if idx < 0 {
			return 0, nil
		}
		end := idx + 4
		result, err := i.engine.DispatchWithState(ctx, i.connID, abi4.PhaseResponseHeaders,
			abi4.FlagFirst|abi4.FlagLast, abi4.Event{Block: i.respHeader[:end]}, i.responseState)
		if err != nil {
			return 1, err
		}
		if result.MutationPresent {
			return 1, fmt.Errorf("%w: response header mutation is not replayable in M5", errBodyMutation)
		}
		length, err := contentLength(i.respHeader[:end])
		if err != nil {
			return 1, err
		}
		i.responseLeft = length
		i.respDone = true
		if result.Verdict != abi4.VerdictAllow {
			return int32(result.Verdict), nil
		}
		body := i.respHeader[end:]
		i.respHeader = nil
		if len(body) == 0 {
			return 0, nil
		}
		return i.inspectResponseBody(ctx, body)
	}
	return i.inspectResponseBody(ctx, chunk)
}

func (i *httpBodyInspector) inspectResponseBody(ctx context.Context, chunk []byte) (int32, error) {
	if i.responseLeft <= 0 || len(chunk) == 0 {
		return 0, nil
	}
	n := len(chunk)
	if int64(n) > i.responseLeft {
		n = int(i.responseLeft)
	}
	result, err := i.engine.DispatchWithState(ctx, i.connID, abi4.PhaseResponseBody, 0,
		abi4.Event{Block: chunk[:n]}, i.responseState)
	if err != nil {
		return 1, err
	}
	if err := applyMutation(chunk[:n], result); err != nil {
		return 1, err
	}
	i.responseLeft -= int64(n)
	return int32(result.Verdict), nil
}

func applyMutation(chunk []byte, result abi4.EventResult) error {
	if !result.MutationPresent {
		return nil
	}
	if len(result.Mutation) != len(chunk) {
		return errBodyMutation
	}
	copy(chunk, result.Mutation)
	return nil
}

func (i *httpBodyInspector) inspect(ctx context.Context, dir int32, chunk []byte) (int32, error) {
	if dir == 0 {
		return i.inspectRequest(ctx, chunk)
	}
	return i.inspectResponse(ctx, chunk)
}
