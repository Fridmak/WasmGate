namespace WasmGate.Control.Core.Configuration;

/// <summary>TLS and mutual-TLS settings for the agent gRPC endpoint.</summary>
/// <remarks>
/// The certificate and key are PEM files. TLS is used only when both are set;
/// with a plain <c>http://</c> listen address the transport stays unencrypted and
/// client certificates are not requested at all.
/// When <see cref="RequireClientCertificate"/> is true,
/// <see cref="ClientCaPath"/> must point to the CA bundle that signed the agent
/// certificates.
/// </remarks>
public sealed class ControlPlaneTlsOptions
{
    /// <summary>PEM file with the control-plane server certificate (and optional chain).</summary>
    public string? CertificatePath { get; init; }

    /// <summary>PEM file with the control-plane server private key.</summary>
    public string? PrivateKeyPath { get; init; }

    /// <summary>PEM bundle of CA certificates trusted for agent client certificates.</summary>
    public string? ClientCaPath { get; init; }

    /// <summary>
    /// Requires every agent to present a client certificate signed by
    /// <see cref="ClientCaPath"/>. Defaults to true.
    /// </summary>
    /// <remarks>
    /// Ignored when <see cref="CertificatePath"/> is not set.
    /// </remarks>
    public bool RequireClientCertificate { get; init; } = true;

    /// <summary>True when a server certificate and key are configured.</summary>
    public bool IsConfigured =>
        !string.IsNullOrWhiteSpace(CertificatePath) &&
        !string.IsNullOrWhiteSpace(PrivateKeyPath);

    /// <summary>
    /// True when any certificate material is configured, even if the pair is
    /// incomplete.
    /// </summary>
    /// <remarks>
    /// Startup validation uses this to reject a partial TLS configuration in
    /// local mode. <see cref="IsConfigured"/> alone would accept a lone
    /// certificate path and defer the failure to Kestrel.
    /// </remarks>
    public bool HasCertificateMaterial =>
        !string.IsNullOrWhiteSpace(CertificatePath) ||
        !string.IsNullOrWhiteSpace(PrivateKeyPath);
}
