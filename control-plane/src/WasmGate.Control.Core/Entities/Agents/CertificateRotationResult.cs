namespace WasmGate.Control.Core.Entities.Agents;

/// <summary>Outcome of rotating an agent certificate.</summary>
/// <param name="CallerActive">
/// True when the caller certificate was active, or when the rotation had no
/// caller certificate. False means the caller was revoked or expired before the
/// transaction and nothing was written.
/// </param>
/// <param name="Retired">Certificates revoked by this rotation, for auditing.</param>
public sealed record CertificateRotationResult(
    bool CallerActive,
    IReadOnlyList<AgentCertificate> Retired);
