namespace WasmGate.Control.Core.Entities;

/// <summary>
/// A certificate issued to a data plane by the control plane's agent CA. Kept
/// so the control plane can track expiry and revoke agents.
/// </summary>
public sealed class AgentCertificate
{
    /// <summary>Agent this certificate belongs to.</summary>
    public required Guid AgentId { get; init; }

    /// <summary>Hex serial number of the certificate.</summary>
    public required string SerialNumber { get; init; }

    /// <summary>SHA-256 fingerprint of the DER-encoded certificate.</summary>
    public required string Thumbprint { get; init; }

    /// <summary>PEM-encoded certificate.</summary>
    public required string Pem { get; init; }

    /// <summary>Start of the validity period.</summary>
    public DateTimeOffset NotBefore { get; init; }

    /// <summary>End of the validity period.</summary>
    public DateTimeOffset NotAfter { get; init; }

    /// <summary>Time when the certificate was issued.</summary>
    public DateTimeOffset IssuedAt { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Time when the certificate was revoked, if it was.</summary>
    public DateTimeOffset? RevokedAt { get; set; }
}
