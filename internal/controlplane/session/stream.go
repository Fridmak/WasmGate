package session

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	"wasmgate/internal/controlplane/identity"
	"wasmgate/internal/controlplane/options"
	"wasmgate/internal/controlplane/release"
	"wasmgate/internal/controlplane/wasmgatepb"
)

// Sender and Receiver are the minimal stream seams used by dispatch tests.
// The generated gRPC session client implements both interfaces.
type Sender interface {
	Send(*wasmgatepb.AgentMessage) error
}

type Receiver interface {
	Recv() (*wasmgatepb.ControlMessage, error)
}

// Run opens the bidirectional stream and runs it until the stream or context
// ends. It does not depend on the root Agent type.
func Run(
	ctx context.Context,
	conn *grpc.ClientConn,
	id *identity.Identity,
	opts options.Options,
	key *ecdsa.PublicKey,
	reloader release.Reloader,
	state *StateStore,
	log *slog.Logger,
) error {
	if log == nil {
		log = slog.Default()
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := wasmgatepb.NewAgentControlClient(conn).Session(sessionCtx)
	if err != nil {
		return fmt.Errorf("control plane: open session: %w", err)
	}

	current := state.Get()
	if err := stream.Send(hello(id.AgentID, opts, current)); err != nil {
		return fmt.Errorf("control plane: send hello: %w", err)
	}
	log.Info("control-plane session opened", "agent_id", id.AgentID, "name", opts.Name)

	outgoing := make(chan *wasmgatepb.AgentMessage, 8)
	sendErr := make(chan error, 1)
	go writeLoop(sessionCtx, stream, outgoing, sendErr)
	go heartbeatLoop(sessionCtx, id.AgentID, opts.Heartbeat, state, outgoing)

	for {
		message, err := stream.Recv()
		if err != nil {
			select {
			case writeFailure := <-sendErr:
				if writeFailure != nil {
					return fmt.Errorf("control plane: send: %w", writeFailure)
				}
			default:
			}
			if errors.Is(err, io.EOF) || sessionCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("control plane: receive: %w", err)
		}
		Dispatch(sessionCtx, conn, message, opts, key, reloader, state, outgoing, log)
	}
}

func writeLoop(ctx context.Context, stream Sender, outgoing <-chan *wasmgatepb.AgentMessage, errCh chan<- error) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-outgoing:
			if err := stream.Send(message); err != nil {
				errCh <- err
				return
			}
		}
	}
}

func heartbeatLoop(ctx context.Context, agentID string, interval time.Duration, state *StateStore, outgoing chan<- *wasmgatepb.AgentMessage) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := state.Get()
			message := &wasmgatepb.AgentMessage{Payload: &wasmgatepb.AgentMessage_Heartbeat{Heartbeat: &wasmgatepb.Heartbeat{
				AgentId: agentID, UnixMs: time.Now().UnixMilli(), ActiveReleaseId: current.ReleaseID, ActiveConfigSha256: current.ConfigSHA256,
			}}}
			select {
			case outgoing <- message:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Dispatch handles one control message and sends an ApplyResult when needed.
func Dispatch(
	ctx context.Context,
	conn *grpc.ClientConn,
	message *wasmgatepb.ControlMessage,
	opts options.Options,
	key *ecdsa.PublicKey,
	reloader release.Reloader,
	state *StateStore,
	outgoing chan<- *wasmgatepb.AgentMessage,
	log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}
	switch payload := message.GetPayload().(type) {
	case *wasmgatepb.ControlMessage_ApplyRelease:
		result := release.Apply(ctx, conn, payload.ApplyRelease, opts, key, reloader, state, log)
		select {
		case outgoing <- &wasmgatepb.AgentMessage{Payload: &wasmgatepb.AgentMessage_ApplyResult{ApplyResult: result}}:
		case <-ctx.Done():
		}
	case *wasmgatepb.ControlMessage_Ping:
		log.Debug("control-plane ping", "nonce", payload.Ping.GetNonce())
	case *wasmgatepb.ControlMessage_Drain:
		log.Warn("drain requested; the data plane keeps serving until the process exits", "reason", payload.Drain.GetReason(), "timeout_ms", payload.Drain.GetTimeoutMs())
	case *wasmgatepb.ControlMessage_SetLogLevel:
		log.Warn("set_log_level is not supported at runtime; restart with -log-level", "level", payload.SetLogLevel.GetLevel())
	case *wasmgatepb.ControlMessage_Rollback:
		log.Warn("rollback is delivered as a normal ApplyRelease; ignoring Rollback", "release_id", payload.Rollback.GetReleaseId())
	default:
		log.Debug("control message without a payload; ignoring")
	}
}

func hello(agentID string, opts options.Options, applied AppliedState) *wasmgatepb.AgentMessage {
	return &wasmgatepb.AgentMessage{Payload: &wasmgatepb.AgentMessage_Hello{Hello: &wasmgatepb.Hello{
		AgentId: agentID, Name: opts.Name, Version: opts.Version, AbiVersions: opts.AbiVersions, Labels: opts.Labels,
		ActiveReleaseId: applied.ReleaseID, ActiveConfigSha256: applied.ConfigSHA256,
	}}}
}
