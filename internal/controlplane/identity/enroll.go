package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"

	"wasmgate/internal/controlplane/options"
	"wasmgate/internal/controlplane/wasmgatepb"
)

// Dialer opens a control-plane gRPC connection with or without a client
// identity. The session package supplies the production implementation.
type Dialer interface {
	Dial(ctx context.Context, id *Identity) (*grpc.ClientConn, error)
}

// enroll requests the first certificate with a one-time token and stores it.
func Enroll(ctx context.Context, opts options.Options, dialer Dialer, log *slog.Logger) (*Identity, error) {
	if strings.TrimSpace(opts.EnrollToken) == "" {
		return nil, fmt.Errorf(
			"no agent certificate in %s and no enrollment token was provided",
			opts.StateDir)
	}

	agentID := ExistingAgentID(opts.StateDir)
	if agentID == "" {
		if provided := strings.TrimSpace(opts.AgentID); provided != "" {
			if !guidPattern.MatchString(provided) {
				return nil, fmt.Errorf("control plane: agent id %q is not a GUID", provided)
			}
			agentID = provided
		}
	}

	if agentID == "" {
		generated, err := NewAgentID()
		if err != nil {
			return nil, err
		}
		agentID = generated
	}

	keyPEM, csrPEM, err := NewKeyAndCSR(agentID)
	if err != nil {
		return nil, err
	}

	conn, err := dialer.Dial(ctx, nil)
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
			Name:    opts.Name,
			Token:   opts.EnrollToken,
			CsrPem:  csrPEM,
		})
	if err != nil {
		return nil, fmt.Errorf("control plane: enroll: %w", err)
	}

	if err := Save(
		opts.StateDir,
		agentID,
		keyPEM,
		response.GetCertificatePem(),
		response.GetCaPem()); err != nil {
		return nil, err
	}

	id, err := Load(opts.StateDir)
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, errors.New("control plane: certificate was not written to the state dir")
	}

	if log == nil {
		log = slog.Default()
	}
	log.Info(
		"agent enrolled",
		"agent_id", agentID,
		"expires_at_unix_ms", response.GetExpiresAtUnixMs())
	return id, nil
}

// renew replaces the client certificate over the existing mTLS identity. The
// private key is rotated as well: a new key and CSR are generated locally.
func Renew(ctx context.Context, opts options.Options, current *Identity, dialer Dialer, log *slog.Logger) (*Identity, error) {
	keyPEM, csrPEM, err := NewKeyAndCSR(current.AgentID)
	if err != nil {
		return nil, err
	}

	conn, err := dialer.Dial(ctx, current)
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

	if err := Save(
		opts.StateDir,
		current.AgentID,
		keyPEM,
		response.GetCertificatePem(),
		caPEM); err != nil {
		return nil, err
	}

	renewed, err := Load(opts.StateDir)
	if err != nil {
		return nil, err
	}
	if renewed == nil {
		return nil, errors.New("control plane: renewed certificate was not written to the state dir")
	}

	if log == nil {
		log = slog.Default()
	}
	log.Info(
		"agent certificate renewed",
		"agent_id", current.AgentID,
		"expires_at", renewed.NotAfter)
	return renewed, nil
}
