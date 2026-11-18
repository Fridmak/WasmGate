using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Api.Infrastructure.Networking;

/// <summary>
/// Refuses configurations that would expose the control plane without
/// protection.
/// </summary>
/// <remarks>
/// The checks run at startup, so a misconfigured deployment fails immediately
/// instead of serving an open admin API or accepting unauthenticated agent
/// sessions over plaintext.
/// </remarks>
internal static class StartupSecurity
{
    /// <summary>Validates the exposure of both listen endpoints.</summary>
    /// <param name="options">Control-plane options to validate.</param>
    /// <exception cref="InvalidOperationException">
    /// The admin API listens on a non-loopback address without a token, the
    /// agent endpoint uses plaintext on a non-loopback address, or local mode is
    /// combined with a non-loopback listener or TLS.
    /// </exception>
    public static void Validate(ControlPlaneOptions options)
    {
        var admin = ListenAddress.Parse(options.HttpListen);
        var grpc = ListenAddress.Parse(options.GrpcListen);

        if (options.Local)
        {
            if (!admin.IsLocal)
            {
                throw new InvalidOperationException(
                    $"WASMGATE_CP_LOCAL requires the admin endpoint on loopback/Unix socket; got {options.HttpListen}.");
            }

            if (!grpc.IsLocal)
            {
                throw new InvalidOperationException(
                    $"WASMGATE_CP_LOCAL requires the agent endpoint on loopback/Unix socket; got {options.GrpcListen}.");
            }

            if (options.GrpcTls.IsConfigured)
            {
                throw new InvalidOperationException(
                    "WASMGATE_CP_LOCAL cannot be combined with TLS/mTLS settings; local mode uses plaintext loopback.");
            }
        }

        if (!admin.IsLocal && string.IsNullOrWhiteSpace(options.AdminToken))
        {
            throw new InvalidOperationException(
                "WASMGATE_CP_ADMIN_TOKEN is required when the admin API listens on a non-loopback address " +
                $"({options.HttpListen}). Set a random secret or bind the admin endpoint to loopback.");
        }

        if (!grpc.IsSecure && !grpc.IsLocal)
        {
            throw new InvalidOperationException(
                "The agent gRPC endpoint must use https:// or listen on loopback/a Unix socket; " +
                $"plaintext on a network address ({options.GrpcListen}) would let anyone impersonate an agent.");
        }
    }
}
