namespace WasmGate.Control.Core.Entities.Agents;

/// <summary>
/// A certificate issued to a data plane by the agent CA. Stored so that issued
/// identities can be listed and revoked.
/// </summary>
/// <remarks>
/// A new row is written on every enrollment; the PEM copy allows an operator to
/// inspect a certificate without reissuing it.
/// </remarks>
public sealed class AgentCertificate
{
    /// <summary>Agent this certificate belongs to.</summary>
    public required Guid AgentId { get; init; }

    /// <summary>Serial number of the certificate as reported by the CA.</summary>
    public required string SerialNumber { get; init; }

    /// <summary>SHA-256 fingerprint of the DER-encoded certificate.</summary>
    public required string Thumbprint { get; init; }

    /// <summary>PEM-encoded certificate.</summary>
    public required string Pem { get; init; }

    /// <summary>Start of the validity period.</summary>
    public DateTimeOffset NotBefore { get; init; }

    /// <summary>End of the validity period; the agent must enroll again afterwards.</summary>
    public DateTimeOffset NotAfter { get; init; }

    /// <summary>Time when the certificate was issued.</summary>
    public DateTimeOffset IssuedAt { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Time when the certificate was revoked, or null while it is active.</summary>
    public DateTimeOffset? RevokedAt { get; set; }
}
