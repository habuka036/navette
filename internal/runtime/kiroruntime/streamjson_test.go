package kiroruntime

import (
	"errors"
	"strings"
	"testing"

	"github.com/habuka036/navette/internal/runtime"
)

// Real stream-json samples captured from kiro-cli 2.21.1 (NAV-TASK-001).
const successStream = `{"type":"runStarted","data":{"payloadSchema":"acp","acpProtocolVersion":1,"engine":"v2"}}
{"type":"metadata","data":{"sessionId":"s1","contextUsagePercentage":3.5}}
{"type":"sessionUpdate","data":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"PO"}}}}
{"type":"sessionUpdate","data":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"NG"}}}}
{"type":"runFinished","data":{"sessionId":"s1","status":"success","stopReason":"end_turn","finalText":"PONG","finalTextTruncated":false}}`

const errorStream = `{"type":"runStarted","data":{"payloadSchema":"acp","acpProtocolVersion":1,"engine":"v2"}}
{"type":"runError","data":{"sessionId":null,"stage":"init","message":"server shut down unexpectedly"}}`

const toolStream = `{"type":"runStarted","data":{"engine":"v2"}}
{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"tool_call","title":"Creating hello.txt","_meta":{"kiro":{"toolName":"write"}}}}}
{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"tool_call_update","title":"Creating hello.txt"}}}
{"type":"runFinished","data":{"status":"success","finalText":"done"}}`

func collect(stream string) ([]runtime.OutputEvent, Result) {
	var evs []runtime.OutputEvent
	res, _ := ParseStream(strings.NewReader(stream), func(e runtime.OutputEvent) error {
		evs = append(evs, e)
		return nil
	})
	return evs, res
}

func TestParseStream_Success(t *testing.T) {
	evs, res := collect(successStream)
	if !res.Succeeded() {
		t.Fatalf("expected success, got %+v", res)
	}
	if res.FinalText != "PONG" {
		t.Errorf("finalText = %q, want PONG", res.FinalText)
	}
	// Message chunks should be emitted.
	var msgs int
	for _, e := range evs {
		if e.Kind == "message" {
			msgs++
		}
	}
	if msgs != 2 {
		t.Errorf("expected 2 message chunks, got %d", msgs)
	}
}

func TestParseStream_Error(t *testing.T) {
	_, res := collect(errorStream)
	if res.Succeeded() {
		t.Fatal("errored stream must not be success")
	}
	if !res.Errored || res.ErrorStage != "init" {
		t.Errorf("expected runError stage=init, got %+v", res)
	}
}

func TestParseStream_ToolEvents(t *testing.T) {
	evs, res := collect(toolStream)
	if !res.Succeeded() {
		t.Fatalf("expected success, got %+v", res)
	}
	var sawToolCall bool
	for _, e := range evs {
		if e.Kind == "tool_call" && strings.Contains(e.Text, "write") {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Error("expected a tool_call event mentioning the write tool")
	}
}

func TestParseStream_MalformedLineSkipped(t *testing.T) {
	stream := "{not json}\n" + successStream
	evs, res := collect(stream)
	if !res.Succeeded() {
		t.Error("malformed leading line must not prevent parsing the rest")
	}
	var sawParseErr bool
	for _, e := range evs {
		if e.Kind == "parse_error" {
			sawParseErr = true
		}
	}
	if !sawParseErr {
		t.Error("expected a parse_error event for the malformed line")
	}
}

func TestParseStream_NoFinishedIsNotSuccess(t *testing.T) {
	stream := `{"type":"runStarted","data":{"engine":"v2"}}
{"type":"metadata","data":{"sessionId":"s1"}}`
	_, res := collect(stream)
	if res.Succeeded() {
		t.Error("a stream without runFinished must not be success")
	}
}

func TestParseStream_EmitErrorAborts(t *testing.T) {
	// emit returns an error on the first event; parsing must stop and return it.
	wantErr := errors.New("persist failed")
	calls := 0
	_, err := ParseStream(strings.NewReader(successStream), func(e runtime.OutputEvent) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected emit error to propagate, got %v", err)
	}
	if calls != 1 {
		t.Errorf("parsing should stop after the first failing emit, got %d calls", calls)
	}
}

func TestParseStream_DuplicateFinishedFailClosed(t *testing.T) {
	stream := successStream + "\n" + `{"type":"runFinished","data":{"status":"success"}}`
	_, err := ParseStream(strings.NewReader(stream), func(e runtime.OutputEvent) error { return nil })
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("duplicate runFinished must be ErrProtocol, got %v", err)
	}
}

func TestParseStream_FinishedThenErrorFailClosed(t *testing.T) {
	stream := successStream + "\n" + `{"type":"runError","data":{"stage":"x","message":"y"}}`
	_, err := ParseStream(strings.NewReader(stream), func(e runtime.OutputEvent) error { return nil })
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("runFinished followed by runError must be ErrProtocol, got %v", err)
	}
}
