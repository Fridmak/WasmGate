package session

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/controlplane/options"
	"wasmgate/internal/controlplane/release"
	"wasmgate/internal/controlplane/wasmgatepb"
)

func TestAppliedStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := LoadApplied(dir); got != (AppliedState{}) {
		t.Fatalf("LoadApplied(empty) = %+v", got)
	}
	want := AppliedState{ReleaseID: "release-1", ConfigSHA256: "abc", Version: 7}
	if err := SaveApplied(dir, want); err != nil {
		t.Fatal(err)
	}
	if got := LoadApplied(dir); got != want {
		t.Fatalf("LoadApplied = %+v, want %+v", got, want)
	}
}

func TestAgentTargetStripsScheme(t *testing.T) {
	if got := Target(options.Options{Endpoint: "https://cp.example.com:9096"}); got != "cp.example.com:9096" {
		t.Fatalf("Target = %q", got)
	}
}

type fakeReloader struct{}

func (fakeReloader) Reload(context.Context, *config.Config) (uint64, error) { return 1, nil }

type fakeSender struct{ messages chan *wasmgatepb.AgentMessage }

func (s *fakeSender) Send(message *wasmgatepb.AgentMessage) error {
	s.messages <- message
	return nil
}

func TestStateStorePersists(t *testing.T) {
	dir := t.TempDir()
	store := NewStateStore(dir, AppliedState{})
	want := AppliedState{ReleaseID: "release-2", ConfigSHA256: "def", Version: 8}
	store.Set(want)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if got := LoadApplied(dir); got != want {
		t.Fatalf("persisted state = %+v, want %+v", got, want)
	}
}

func TestWriteLoopSendsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *wasmgatepb.AgentMessage, 1)
	sent := &fakeSender{messages: make(chan *wasmgatepb.AgentMessage, 1)}
	errCh := make(chan error, 1)
	go writeLoop(ctx, sent, out, errCh)

	msg := &wasmgatepb.AgentMessage{}
	out <- msg
	select {
	case got := <-sent.messages:
		if got != msg {
			t.Fatalf("writeLoop sent %p, want %p", got, msg)
		}
	case <-time.After(time.Second):
		t.Fatal("writeLoop did not send the queued message")
	}
	cancel()
}

func TestHeartbeatLoopReportsAppliedState(t *testing.T) {
	state := NewStateStore("", AppliedState{ReleaseID: "release-1", ConfigSHA256: "sha"})
	out := make(chan *wasmgatepb.AgentMessage, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go heartbeatLoop(ctx, "agent-1", time.Millisecond, state, out)

	select {
	case got := <-out:
		hb := got.GetHeartbeat()
		if hb.GetAgentId() != "agent-1" || hb.GetActiveReleaseId() != "release-1" || hb.GetActiveConfigSha256() != "sha" {
			t.Fatalf("heartbeat = %+v", hb)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeatLoop did not emit a heartbeat")
	}
}

func TestDispatchHandlesAllControlMessages(t *testing.T) {
	opts := options.Options{}
	reloader := fakeReloader{}
	cases := []struct {
		name string
		msg  *wasmgatepb.ControlMessage
		want bool
	}{
		{"ping", &wasmgatepb.ControlMessage{Payload: &wasmgatepb.ControlMessage_Ping{Ping: &wasmgatepb.Ping{Nonce: "n"}}}, false},
		{"drain", &wasmgatepb.ControlMessage{Payload: &wasmgatepb.ControlMessage_Drain{Drain: &wasmgatepb.Drain{Reason: "test"}}}, false},
		{"rollback", &wasmgatepb.ControlMessage{Payload: &wasmgatepb.ControlMessage_Rollback{Rollback: &wasmgatepb.Rollback{ReleaseId: "r"}}}, false},
		{"apply", &wasmgatepb.ControlMessage{Payload: &wasmgatepb.ControlMessage_ApplyRelease{ApplyRelease: &wasmgatepb.ApplyRelease{ReleaseId: "r"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := make(chan *wasmgatepb.AgentMessage, 1)
			sender := &fakeSender{messages: make(chan *wasmgatepb.AgentMessage, 1)}
			go writeLoop(ctx, sender, out, make(chan error, 1))
			Dispatch(ctx, nil, tc.msg, opts, nil, reloader, NewStateStore("", AppliedState{}), out, slog.Default())
			if tc.want {
				select {
				case got := <-sender.messages:
					if got.GetApplyResult().GetOk() {
						t.Fatal("unsigned apply unexpectedly succeeded")
					}
				case <-time.After(time.Second):
					t.Fatal("apply did not send an ApplyResult")
				}
			} else if len(sender.messages) != 0 {
				t.Fatalf("%s emitted an unexpected message", tc.name)
			}
		})
	}
}

var _ release.Reloader = fakeReloader{}
