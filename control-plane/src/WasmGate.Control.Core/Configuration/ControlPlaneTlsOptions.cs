namespace WasmGate.Control.Core.Configuration;

/// <summary>TLS and mutual-TLS settings for the agent gRPC endpoint.</summary>
/// <remarks>
/// The certificate and key are PEM files. When
/// <see cref="RequireClientCertificate"/> is true, <see cref="ClientCaPath"/>
/// must point to the CA bundle used to validate agent certificates.
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
    public bool RequireClientCertificate { get; init; } = true;

    /// <summary>True when a server certificate and key are configured.</summary>
    public bool IsConfigured =>
        !string.IsNullOrWhiteSpace(CertificatePath) &&
        !string.IsNullOrWhiteSpace(PrivateKeyPath);
}
