package session

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"wasmgate/internal/controlplane/identity"
	"wasmgate/internal/controlplane/options"
)

// Dialer is the production identity.Dialer implementation used by enrollment
// and renewal.
type Dialer struct{ Options options.Options }

// dial opens a gRPC connection. A nil identity is used for enrollment, when the
// agent has no client certificate yet.
func (d Dialer) Dial(ctx context.Context, id *identity.Identity) (*grpc.ClientConn, error) {
	_ = ctx
	creds, err := TransportCredentials(d.Options, id)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(
		Target(d.Options),
		grpc.WithTransportCredentials(creds),
		grpc.WithUserAgent("wasmgate-dp/"+d.Options.Version),
	)
	if err != nil {
		return nil, fmt.Errorf("control plane: dial %s: %w", d.Options.Endpoint, err)
	}
	return conn, nil
}

// target is the endpoint without its URL scheme, which is what gRPC expects
// ("host:port"); the scheme only selects the transport credentials.
func Target(opts options.Options) string {
	u, err := url.Parse(opts.Endpoint)
	if err != nil || u.Host == "" {
		return opts.Endpoint
	}
	return u.Host
}

// transportCredentials selects plaintext (local development) or mTLS.
func TransportCredentials(opts options.Options, id *identity.Identity) (credentials.TransportCredentials, error) {
	if strings.HasPrefix(strings.ToLower(opts.Endpoint), "http://") {
		return insecure.NewCredentials(), nil
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: opts.ServerName,
	}
	if id != nil {
		tlsConfig.Certificates = []tls.Certificate{id.Certificate}
	}

	if opts.ServerCA != "" {
		caPEM, err := os.ReadFile(opts.ServerCA) // #nosec G304 -- operator-configured path
		if err != nil {
			return nil, fmt.Errorf("control plane: read server CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("control plane: %s contains no PEM certificates", opts.ServerCA)
		}
		tlsConfig.RootCAs = pool
	}

	return credentials.NewTLS(tlsConfig), nil
}
