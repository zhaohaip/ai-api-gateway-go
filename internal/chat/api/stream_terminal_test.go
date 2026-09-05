package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func TestStreamTerminalDecisionKeepsWinnerAndCause(t *testing.T) {
	for _, pair := range [][2]string{{"complete", "idle"}, {"complete", "total"}, {"complete", "client"}, {"complete", "error"}, {"idle", "error"}, {"total", "client"}} {
		t.Run(pair[0]+" versus "+pair[1], func(t *testing.T) {
			for _, order := range []string{"left first", "right first", "simultaneous"} {
				t.Run(order, func(t *testing.T) {
					parent, cancel := context.WithCancelCause(context.Background())
					clock := newControlledTimerFactory()
					state := newStreamTimeoutState(parent, StreamTimeouts{FirstChunk: time.Second, Idle: 30 * time.Second, Total: time.Minute}, clock.New)
					t.Cleanup(func() { cancel(context.Canceled); state.Close() })
					state.FirstChunkReceived()
					state.OutputSent()
					idle, total := clock.Wait(t, 2), clock.Wait(t, 0)
					clientErr, readErr := errors.New("client disconnected"), errors.New("stream read failed")
					events := map[string]func(){
						"complete": func() { state.decide(streamCompleted, nil) },
						"idle":     func() { idle.Trigger() },
						"total":    func() { total.Trigger() },
						"client":   func() { cancel(clientErr); state.result() },
						"error":    func() { state.decide(streamFailed, readErr) },
					}
					wantKind := map[string]streamTerminal{"complete": streamCompleted, "idle": streamTimedOut, "total": streamTimedOut, "client": streamCanceled, "error": streamFailed}
					switch order {
					case "left first":
						events[pair[0]]()
						events[pair[1]]()
					case "right first":
						events[pair[1]]()
						events[pair[0]]()
					default:
						runBoundaryRace(t, events[pair[0]], events[pair[1]])
					}
					kind, cause := state.result()
					winner := ""
					switch kind {
					case streamCompleted:
						winner = "complete"
						if cause != context.Canceled {
							t.Fatalf("completed cause=%v", cause)
						}
					case streamTimedOut:
						timeout := timeoutFromContext(state.Context())
						if timeout == nil {
							t.Fatalf("timeout cause=%v", cause)
						}
						switch timeout.typeName {
						case timeoutTypeStreamIdle:
							winner = "idle"
							if timeout.duration != 30*time.Second {
								t.Fatal("idle duration changed")
							}
						case timeoutTypeStreamTotal:
							winner = "total"
							if timeout.duration != time.Minute {
								t.Fatal("total duration changed")
							}
						default:
							t.Fatalf("unexpected timeout=%v", timeout)
						}
					case streamCanceled:
						winner = "client"
						if cause != clientErr {
							t.Fatalf("client cause=%v", cause)
						}
					case streamFailed:
						winner = "error"
						if cause != readErr {
							t.Fatalf("error cause=%v", cause)
						}
					default:
						t.Fatalf("no terminal decision: %v", kind)
					}
					if winner != pair[0] && winner != pair[1] {
						t.Fatalf("unrelated event won: %s", winner)
					}
					if order == "left first" && (winner != pair[0] || kind != wantKind[pair[0]]) {
						t.Fatalf("left event lost: %s", winner)
					}
					if order == "right first" && (winner != pair[1] || kind != wantKind[pair[1]]) {
						t.Fatalf("right event lost: %s", winner)
					}
					assertTimersStopped(t, clock)
					// 模拟 Stop 前已调度的迟到回调；终态及 Cause 均不得改变。
					runBoundaryRace(t, func() { idle.callback(); total.callback() }, func() {
						cancel(errors.New("late client"))
						state.decide(streamFailed, errors.New("late error"))
						state.Close()
					})
					state.OutputSent()
					if state.decide(streamCompleted, nil) {
						t.Fatal("second terminal decision won")
					}
					finalKind, finalCause := state.result()
					if finalKind != kind || finalCause != cause {
						t.Fatalf("winner changed: (%v,%v) -> (%v,%v)", kind, cause, finalKind, finalCause)
					}
					state.Close()
					select {
					case <-state.parentDone:
					default:
						t.Fatal("parent callback not reclaimed")
					}
					assertTimersStopped(t, clock)
				})
			}
		})
	}
}

func TestStreamTerminalParentCancellationBeforeRegistration(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			parent, cancel := context.WithCancelCause(context.WithValue(context.Background(), streamRequestContextKey{}, "value"))
			cancel(cause)
			clock := newControlledTimerFactory()
			state := newStreamTimeoutState(parent, StreamTimeouts{FirstChunk: time.Second, Total: time.Minute}, clock.New)
			defer state.Close()
			kind, got := state.result()
			if kind != streamCanceled || got != cause || state.Context().Value(streamRequestContextKey{}) != "value" {
				t.Fatalf("terminal=%v cause=%v", kind, got)
			}
			if state.decide(streamCompleted, nil) {
				t.Fatal("completed after parent cancellation")
			}
			assertTimersStopped(t, clock)
		})
	}
}

func TestSSECompletedDecisionRejectsLateTimeoutAndClientCancellation(t *testing.T) {
	for _, event := range []string{"idle", "total", "client"} {
		t.Run(event, func(t *testing.T) {
			var r *boundaryRun
			var before string
			r = startBoundaryStreamWithWriter(t, StreamTimeouts{Idle: time.Minute, Total: 5 * time.Minute}, func(w *flushRecorder) http.ResponseWriter {
				return &boundaryDoneWriter{flushRecorder: w, beforeDone: func() {
					before = w.Body.String()
					switch event {
					case "idle":
						r.clock.Wait(t, 1).callback()
					case "total":
						r.clock.Wait(t, 0).callback()
					case "client":
						r.cancel(errors.New("client canceled during DONE write"))
					}
				}}
			})
			r.send(t, contentChunk("retained"))
			r.send(t, domain.ChatChunk{FinishReason: stringPointer("stop")})
			r.finish(t)
			if context.Cause(r.stream.ctx) != context.Canceled {
				t.Fatalf("late %s changed completion cause: %v", event, context.Cause(r.stream.ctx))
			}
			if !strings.Contains(before, `"content":"retained"`) || r.writer.Body.String() != before+"data: [DONE]\n\n" || r.writer.flushCount != 3 {
				t.Fatalf("completed output changed: %s", r.writer.Body.String())
			}
		})
	}
}

func TestSSEAbnormalTerminalBeforeEOFNeverSendsDone(t *testing.T) {
	for _, event := range []string{"idle", "total", "client", "error"} {
		t.Run(event, func(t *testing.T) {
			r := startBoundaryStream(t, StreamTimeouts{Idle: time.Minute, Total: 5 * time.Minute})
			r.send(t, contentChunk("retained"))
			r.send(t, domain.ChatChunk{FinishReason: stringPointer("stop")})
			before := r.writer.Body.String()
			cause := errors.New("terminal " + event)
			switch event {
			case "idle":
				r.clock.Wait(t, 1).Trigger()
			case "total":
				r.clock.Wait(t, 0).Trigger()
			case "client":
				r.cancel(cause)
			case "error":
				r.stream.results <- streamResult{err: cause}
			}
			r.waitDone(t)
			switch event {
			case "idle":
				assertBoundaryCause(t, r.stream.ctx, timeoutTypeStreamIdle)
			case "total":
				assertBoundaryCause(t, r.stream.ctx, timeoutTypeStreamTotal)
			default:
				if !errors.Is(context.Cause(r.stream.ctx), cause) {
					t.Fatalf("cause=%v, want %v", context.Cause(r.stream.ctx), cause)
				}
			}
			if r.writer.Body.String() != before || strings.Contains(before, doneEvent) || strings.Contains(before, `"error"`) || r.writer.flushCount != 2 {
				t.Fatalf("abnormal terminal changed output: %s", r.writer.Body.String())
			}
		})
	}
}
