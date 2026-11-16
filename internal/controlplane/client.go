package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"wasmgate/internal/config"
	"wasmgate/internal/controlplane/wasmgatepb"
)

// Reloader applies a new configuration to the running data plane. It is
// implemented by proxy.Server.
type Reloader interface {
	Reload(ctx context.Context, cfg *config.Config) (uint64, error)
}

// Agent keeps one data plane connected to the control plane.
type Agent struct {
	opts       Options
	log        *slog.Logger
	reloader   Reloader
	releaseKey *ecdsa.PublicKey
}

// NewAgent validates opts and builds a control-plane client.
func NewAgent(opts Options, reloader Reloader, log *slog.Logger) (*Agent, error) {
	if reloader == nil {
		return nil, errors.New("control plane: reloader is required")
	}
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	releaseKey, err := loadReleaseKey(opts.ReleaseKey)
	if err != nil {
		return nil, err
	}

	return &Agent{opts: opts, log: log, reloader: reloader, releaseKey: releaseKey}, nil
}

// Run keeps the session alive until ctx is cancelled. Connection failures are
// retried with an exponential backoff; a cancelled context returns nil.
func (a *Agent) Run(ctx context.Context) error {
	state := &stateStore{st: loadApplied(a.opts.StateDir)}
	backoff := a.opts.ReconnectMin

	for {
		if ctx.Err() != nil {
			return nil
		}

		started := time.Now()
		err := a.runOnce(ctx, state)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			a.log.Warn("control-plane session ended", "err", err)
		} else {
			a.log.Info("control-plane session closed")
		}

		// A session that stayed up for a while means the endpoint is healthy:
		// start the backoff from scratch instead of doubling it forever.
		if time.Since(started) >= a.opts.ReconnectMax {
			backoff = a.opts.ReconnectMin
		}

		a.log.Info("reconnecting to control plane", "endpoint", a.opts.Endpoint, "in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		backoff *= 2
		if backoff > a.opts.ReconnectMax {
			backoff = a.opts.ReconnectMax
		}
	}
}

// runOnce renews or enrolls the identity when needed, opens one session and
// returns when the session ends.
func (a *Agent) runOnce(ctx context.Context, state *stateStore) error {
	id, err := loadIdentity(a.opts.StateDir)
	switch {
	case errors.Is(err, errIncompleteIdentity):
		// A crash between file writes leaves a half-written identity. Enrolling
		// again rewrites all four files instead of blocking forever.
		a.log.Warn("incomplete agent identity, enrolling again", "state_dir", a.opts.StateDir)
		id, err = a.enroll(ctx)
		if err != nil {
			return fmt.Errorf("enrollment: %w", err)
		}
	case err != nil:
		return err
	case id == nil:
		id, err = a.enroll(ctx)
		if err != nil {
			return fmt.Errorf("enrollment: %w", err)
		}
	}

	if id != nil && !strings.HasPrefix(strings.ToLower(a.opts.Endpoint), "http://") {
		remaining := time.Until(id.NotAfter)
		switch {
		case remaining <= 0:
			// An expired certificate cannot authenticate, so renewal is impossible:
			// replace it by enrolling again.
			a.log.Error("agent certificate expired", "expired_at", id.NotAfter)
			if strings.TrimSpace(a.opts.EnrollToken) == "" {
				return fmt.Errorf(
					"control plane: certificate expired at %s; issue an enrollment token and restart",
					id.NotAfter.Format(time.RFC3339))
			}

			id, err = a.enroll(ctx)
			if err != nil {
				return fmt.Errorf("re-enrollment: %w", err)
			}
		case remaining < a.opts.RenewBefore:
			renewed, renewErr := a.renew(ctx, id)
			if renewErr != nil {
				a.log.Warn(
					"certificate renewal failed; continuing with the current certificate",
					"err", renewErr,
					"expires_at", id.NotAfter)
			} else {
				id = renewed
			}
		}
	}

	conn, err := a.dial(id)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // closing a client connection has no recovery path

	return a.session(ctx, conn, id, state)
}

// dial opens a gRPC connection. A nil identity is used for enrollment, when the
// agent has no client certificate yet.
func (a *Agent) dial(id *identity) (*grpc.ClientConn, error) {
	creds, err := a.transportCredentials(id)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(
		a.target(),
		grpc.WithTransportCredentials(creds),
		grpc.WithUserAgent("wasmgate-dp/"+a.opts.Version),
	)
	if err != nil {
		return nil, fmt.Errorf("control plane: dial %s: %w", a.opts.Endpoint, err)
	}
	return conn, nil
}

// target is the endpoint without its URL scheme, which is what gRPC expects
// ("host:port"); the scheme only selects the transport credentials.
func (a *Agent) target() string {
	u, err := url.Parse(a.opts.Endpoint)
	if err != nil || u.Host == "" {
		return a.opts.Endpoint
	}
	return u.Host
}

// transportCredentials selects plaintext (local development) or mTLS.
func (a *Agent) transportCredentials(id *identity) (credentials.TransportCredentials, error) {
	if strings.HasPrefix(strings.ToLower(a.opts.Endpoint), "http://") {
		return insecure.NewCredentials(), nil
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: a.opts.ServerName,
	}
	if id != nil {
		tlsConfig.Certificates = []tls.Certificate{id.Certificate}
	}

	if a.opts.ServerCA != "" {
		caPEM, err := os.ReadFile(a.opts.ServerCA) // #nosec G304 -- operator-configured path
		if err != nil {
			return nil, fmt.Errorf("control plane: read server CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("control plane: %s contains no PEM certificates", a.opts.ServerCA)
		}
		tlsConfig.RootCAs = pool
	}

	return credentials.NewTLS(tlsConfig), nil
}

// enroll requests the first certificate with a one-time token and stores it.
func (a *Agent) enroll(ctx context.Context) (*identity, error) {
	if strings.TrimSpace(a.opts.EnrollToken) == "" {
		return nil, fmt.Errorf(
			"no agent certificate in %s and no enrollment token was provided",
			a.opts.StateDir)
	}

	agentID := existingAgentID(a.opts.StateDir)
	if agentID == "" {
		if provided := strings.TrimSpace(a.opts.AgentID); provided != "" {
			if !guidPattern.MatchString(provided) {
				return nil, fmt.Errorf("control plane: agent id %q is not a GUID", provided)
			}
			agentID = provided
		}
	}

	if agentID == "" {
		generated, err := newAgentID()
		if err != nil {
			return nil, err
		}
		agentID = generated
	}

	keyPEM, csrPEM, err := newKeyAndCSR(agentID)
	if err != nil {
		return nil, err
	}

	conn, err := a.dial(nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // closing a client connection has no recovery path

	enrollCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	response, err := wasmgatepb.NewAgentEnrollmentClient(conn).Enroll(
		enrollCtx,
		&wasmgatepb.EnrollRequest{
			AgentId: agentID,
			Name:    a.opts.Name,
			Token:   a.opts.EnrollToken,
			CsrPem:  csrPEM,
		})
	if err != nil {
		return nil, fmt.Errorf("control plane: enroll: %w", err)
	}

	if err := saveIdentity(
		a.opts.StateDir,
		agentID,
		keyPEM,
		response.GetCertificatePem(),
		response.GetCaPem()); err != nil {
		return nil, err
	}

	id, err := loadIdentity(a.opts.StateDir)
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, errors.New("control plane: certificate was not written to the state dir")
	}

	a.log.Info(
		"agent enrolled",
		"agent_id", agentID,
		"expires_at_unix_ms", response.GetExpiresAtUnixMs())
	return id, nil
}

// renew replaces the client certificate over the existing mTLS identity. The
// private key is rotated as well: a new key and CSR are generated locally.
func (a *Agent) renew(ctx context.Context, current *identity) (*identity, error) {
	keyPEM, csrPEM, err := newKeyAndCSR(current.AgentID)
	if err != nil {
		return nil, err
	}

	conn, err := a.dial(current)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // closing a client connection has no recovery path

	renewCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	response, err := wasmgatepb.NewAgentEnrollmentClient(conn).Renew(
		renewCtx,
		&wasmgatepb.RenewRequest{
			AgentId: current.AgentID,
			CsrPem:  csrPEM,
		})
	if err != nil {
		return nil, fmt.Errorf("control plane: renew: %w", err)
	}

	caPEM := response.GetCaPem()
	if len(caPEM) == 0 {
		caPEM = current.CAPEM
	}

	if err := saveIdentity(
		a.opts.StateDir,
		current.AgentID,
		keyPEM,
		response.GetCertificatePem(),
		caPEM); err != nil {
		return nil, err
	}

	renewed, err := loadIdentity(a.opts.StateDir)
	if err != nil {
		return nil, err
	}
	if renewed == nil {
		return nil, errors.New("control plane: renewed certificate was not written to the state dir")
	}

	a.log.Info(
		"agent certificate renewed",
		"agent_id", current.AgentID,
		"expires_at", renewed.NotAfter)
	return renewed, nil
}

// session opens the bidirectional stream and runs it until it ends.
func (a *Agent) session(
	ctx context.Context,
	conn *grpc.ClientConn,
	id *identity,
	state *stateStore,
) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := wasmgatepb.NewAgentControlClient(conn).Session(sessionCtx)
	if err != nil {
		return fmt.Errorf("control plane: open session: %w", err)
	}

	current := state.get()
	if err := stream.Send(hello(id.AgentID, a.opts, current)); err != nil {
		return fmt.Errorf("control plane: send hello: %w", err)
	}
	a.log.Info("control-plane session opened", "agent_id", id.AgentID, "name", a.opts.Name)

	outgoing := make(chan *wasmgatepb.AgentMessage, 8)
	sendErr := make(chan error, 1)

	go writeLoop(sessionCtx, stream, outgoing, sendErr)
	go heartbeatLoop(sessionCtx, id.AgentID, a.opts.Heartbeat, state, outgoing)

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

		a.dispatch(sessionCtx, conn, message, state, outgoing)
	}
}

// writeLoop serializes every write to the stream: gRPC does not allow
// concurrent Send calls, and both heartbeats and apply results write.
func writeLoop(
	ctx context.Context,
	stream wasmgatepb.AgentControl_SessionClient,
	outgoing <-chan *wasmgatepb.AgentMessage,
	errCh chan<- error,
) {
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

// heartbeatLoop reports liveness and the applied release on a fixed interval.
func heartbeatLoop(
	ctx context.Context,
	agentID string,
	interval time.Duration,
	state *stateStore,
	outgoing chan<- *wasmgatepb.AgentMessage,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := state.get()
			message := &wasmgatepb.AgentMessage{
				Payload: &wasmgatepb.AgentMessage_Heartbeat{
					Heartbeat: &wasmgatepb.Heartbeat{
						AgentId:            agentID,
						UnixMs:             time.Now().UnixMilli(),
						ActiveReleaseId:    current.ReleaseID,
						ActiveConfigSha256: current.ConfigSHA256,
					},
				},
			}

			select {
			case outgoing <- message:
			case <-ctx.Done():
				return
			}
		}
	}
}

// dispatch handles one control message from the control plane.
func (a *Agent) dispatch(
	ctx context.Context,
	conn *grpc.ClientConn,
	message *wasmgatepb.ControlMessage,
	state *stateStore,
	outgoing chan<- *wasmgatepb.AgentMessage,
) {
	switch payload := message.GetPayload().(type) {
	case *wasmgatepb.ControlMessage_ApplyRelease:
		result := a.applyRelease(ctx, conn, payload.ApplyRelease, state)
		select {
		case outgoing <- &wasmgatepb.AgentMessage{
			Payload: &wasmgatepb.AgentMessage_ApplyResult{ApplyResult: result},
		}:
		case <-ctx.Done():
		}

	case *wasmgatepb.ControlMessage_Ping:
		a.log.Debug("control-plane ping", "nonce", payload.Ping.GetNonce())

	case *wasmgatepb.ControlMessage_Drain:
		a.log.Warn(
			"drain requested; the data plane keeps serving until the process exits",
			"reason", payload.Drain.GetReason(),
			"timeout_ms", payload.Drain.GetTimeoutMs())

	case *wasmgatepb.ControlMessage_SetLogLevel:
		a.log.Warn(
			"set_log_level is not supported at runtime; restart with -log-level",
			"level", payload.SetLogLevel.GetLevel())

	case *wasmgatepb.ControlMessage_Rollback:
		a.log.Warn(
			"rollback is delivered as a normal ApplyRelease; ignoring Rollback",
			"release_id", payload.Rollback.GetReleaseId())

	default:
		a.log.Debug("control message without a payload; ignoring")
	}
}

// hello builds the first message of a session.
func hello(agentID string, opts Options, applied appliedState) *wasmgatepb.AgentMessage {
	return &wasmgatepb.AgentMessage{
		Payload: &wasmgatepb.AgentMessage_Hello{
			Hello: &wasmgatepb.Hello{
				AgentId:            agentID,
				Name:               opts.Name,
				Version:            opts.Version,
				AbiVersions:        opts.AbiVersions,
				Labels:             opts.Labels,
				ActiveReleaseId:    applied.ReleaseID,
				ActiveConfigSha256: applied.ConfigSHA256,
			},
		},
	}
}

// appliedState is the release the data plane currently runs. It is persisted so
// Hello and Heartbeat can report it immediately after a restart.
type appliedState struct {
	ReleaseID    string `json:"release_id,omitempty"`
	ConfigSHA256 string `json:"config_sha256,omitempty"`
	Version      uint64 `json:"config_version,omitempty"`
}

// stateStore guards the applied state shared with the heartbeat goroutine.
type stateStore struct {
	mu sync.RWMutex
	st appliedState
}

// get returns a copy of the current state.
func (s *stateStore) get() appliedState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st
}

// set replaces the current state.
func (s *stateStore) set(st appliedState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = st
}

// appliedStateFile is the file name used to persist appliedState.
const appliedStateFile = "applied.json"

// loadApplied reads the persisted state, returning the zero value when it is
// missing or unreadable: a fresh agent simply has no active release yet.
func loadApplied(dir string) appliedState {
	var st appliedState
	raw, err := os.ReadFile(filepath.Join(dir, appliedStateFile)) // #nosec G304
	if err != nil {
		return st
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return appliedState{}
	}
	return st
}

// saveApplied persists the state for the next start.
func saveApplied(dir string, st appliedState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("control plane: marshal applied state: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("control plane: create state dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, appliedStateFile), raw, 0o644); err != nil { // #nosec G306 -- applied state is not secret
		return fmt.Errorf("control plane: write applied state: %w", err)
	}
	return nil
}
