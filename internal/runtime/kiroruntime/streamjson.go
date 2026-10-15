package kiroruntime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/habuka036/navette/internal/runtime"
)

// streamEvent is the top-level envelope of a Kiro CLI stream-json line:
// {"type": "...", "data": {...}}. Confirmed against kiro-cli 2.21.1 (see
// docs/specs/harness/notes/milestone-c-sandbox.md and milestone-a-experiments.md).
type streamEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// runFinishedData is the payload of a runFinished event.
type runFinishedData struct {
	SessionID  string `json:"sessionId"`
	Status     string `json:"status"` // "success", etc.
	StopReason string `json:"stopReason"`
	FinalText  string `json:"finalText"`
}

// runErrorData is the payload of a runError event.
type runErrorData struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

// sessionUpdateData is the payload of a sessionUpdate event. We extract the
// nested update discriminator and a best-effort text/kind for logging.
type sessionUpdateData struct {
	Update struct {
		SessionUpdate string `json:"sessionUpdate"` // agent_message_chunk | tool_call | tool_call_update
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Title string `json:"title"`
		Meta  struct {
			Kiro struct {
				ToolName string `json:"toolName"`
			} `json:"kiro"`
		} `json:"_meta"`
	} `json:"update"`
}

// Result is the parsed terminal outcome of a stream-json run.
type Result struct {
	// Finished is true when a runFinished event was seen.
	Finished bool
	// Status is runFinished.status (e.g. "success"). Empty if not finished.
	Status string
	// FinalText is runFinished.finalText.
	FinalText string
	// Errored is true when a runError event was seen.
	Errored bool
	// ErrorStage/ErrorMessage are from runError.
	ErrorStage   string
	ErrorMessage string
}

// Succeeded reports whether the run finished with status "success" and no error.
func (r Result) Succeeded() bool {
	return r.Finished && r.Status == "success" && !r.Errored
}

// ErrProtocol indicates a malformed/contradictory stream-json terminal sequence
// (e.g. duplicate runFinished, or both runFinished and runError). It is
// fail-closed: the run must not be treated as successful.
var ErrProtocol = errors.New("kiroruntime: stream-json protocol violation")

// ParseStream reads newline-delimited JSON from r, emitting a runtime output
// event per meaningful line via emit, and returns the parsed terminal Result.
//
// Success is determined by runFinished.status / runError — NEVER by process exit
// code. Terminal-event anomalies are fail-closed: a second runFinished, or a
// runFinished together with a runError, yields ErrProtocol. Malformed JSON lines
// are surfaced as "parse_error" output events and skipped (they do not abort
// parsing). If emit returns an error, parsing stops and that error is returned
// (persistence failure is fatal; the caller must not report success).
func ParseStream(r io.Reader, emit func(runtime.OutputEvent) error) (Result, error) {
	var res Result
	if emit == nil {
		emit = func(runtime.OutputEvent) error { return nil }
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if eerr := emit(runtime.OutputEvent{Kind: "parse_error", Text: truncateForLog(string(line))}); eerr != nil {
				return res, eerr
			}
			continue
		}
		switch ev.Type {
		case "runFinished":
			if res.Finished || res.Errored {
				return res, fmt.Errorf("%w: duplicate/contradictory terminal event (runFinished)", ErrProtocol)
			}
			var d runFinishedData
			_ = json.Unmarshal(ev.Data, &d)
			res.Finished = true
			res.Status = d.Status
			res.FinalText = d.FinalText
			if eerr := emit(runtime.OutputEvent{Kind: "runFinished", Text: d.Status}); eerr != nil {
				return res, eerr
			}
		case "runError":
			if res.Finished || res.Errored {
				return res, fmt.Errorf("%w: duplicate/contradictory terminal event (runError)", ErrProtocol)
			}
			var d runErrorData
			_ = json.Unmarshal(ev.Data, &d)
			res.Errored = true
			res.ErrorStage = d.Stage
			res.ErrorMessage = d.Message
			if eerr := emit(runtime.OutputEvent{Kind: "runError", Text: fmt.Sprintf("%s: %s", d.Stage, d.Message)}); eerr != nil {
				return res, eerr
			}
		case "sessionUpdate":
			if eerr := emit(decodeSessionUpdate(ev.Data)); eerr != nil {
				return res, eerr
			}
		case "runStarted", "metadata":
			if ev.Type == "runStarted" {
				if eerr := emit(runtime.OutputEvent{Kind: "runStarted"}); eerr != nil {
					return res, eerr
				}
			}
		default:
			if eerr := emit(runtime.OutputEvent{Kind: ev.Type}); eerr != nil {
				return res, eerr
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("kiroruntime.ParseStream: scan: %w", err)
	}
	return res, nil
}

func decodeSessionUpdate(data json.RawMessage) runtime.OutputEvent {
	var d sessionUpdateData
	if err := json.Unmarshal(data, &d); err != nil {
		return runtime.OutputEvent{Kind: "sessionUpdate"}
	}
	switch d.Update.SessionUpdate {
	case "agent_message_chunk":
		return runtime.OutputEvent{Kind: "message", Text: d.Update.Content.Text}
	case "tool_call":
		name := d.Update.Meta.Kiro.ToolName
		return runtime.OutputEvent{Kind: "tool_call", Text: fmt.Sprintf("%s: %s", name, d.Update.Title)}
	case "tool_call_update":
		return runtime.OutputEvent{Kind: "tool_call_update", Text: d.Update.Title}
	default:
		return runtime.OutputEvent{Kind: "sessionUpdate", Text: d.Update.SessionUpdate}
	}
}

func truncateForLog(s string) string {
	const max = 512
	if len(s) > max {
		return s[:max] + "...(truncated)"
	}
	return s
}
