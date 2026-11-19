using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>Stores the client certificates issued to data planes.</summary>
/// <remarks>
/// A fresh certificate is recorded on every enrollment, so one agent can hold
/// several; revocation is recorded per certificate and never deletes the row.
/// </remarks>
public interface IAgentCertificateRepository
{
    /// <summary>Stores an issued certificate.</summary>
    /// <param name="certificate">Certificate to persist, including the PEM copy.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken);

    /// <summary>Lists certificates of an agent that were not revoked.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Certificates without a revocation timestamp.</returns>
    Task<IReadOnlyList<AgentCertificate>> ListActiveByAgentAsync(
        Guid agentId,
        CancellationToken cancellationToken);

    /// <summary>Checks that a certificate is known, active and not expired.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="thumbprint">SHA-256 fingerprint, upper-case hex, of the DER certificate.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>True when the control plane issued the certificate and has not revoked it.</returns>
    /// <remarks>
    /// Used at session start, because the TLS layer cannot consult a CRL: a
    /// revoked certificate must stop working without waiting for its expiry.
    /// </remarks>
    Task<bool> IsActiveAsync(Guid agentId, string thumbprint, CancellationToken cancellationToken);

    /// <summary>Marks one certificate of an agent as revoked.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="serialNumber">Certificate serial number.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>True when an active certificate was found and revoked.</returns>
    /// <remarks>The record is kept for audit.</remarks>
    Task<bool> RevokeAsync(Guid agentId, string serialNumber, CancellationToken cancellationToken);

    /// <summary>
    /// Revokes every other active certificate of an agent and stores a new one
    /// in the same transaction.
    /// </summary>
    /// <param name="certificate">Replacement certificate to store.</param>
    /// <param name="callerThumbprint">
    /// Fingerprint of the certificate that requested the rotation, or null for
    /// enrollment and recovery. When set, the certificate must still be active
    /// inside the transaction; otherwise nothing is written.
    /// </param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// Whether the caller was active and the certificates retired by the rotation.
    /// </returns>
    /// <remarks>
    /// Immediate rotation is deliberate: once the new certificate is issued,
    /// the old identity must no longer be accepted. The caller closes any live
    /// session using one of the retired certificates after the transaction. The
    /// in-transaction caller check closes the TOCTOU window between a session's
    /// active check and the rotation commit.
    /// </remarks>
    Task<CertificateRotationResult> RotateAsync(
        AgentCertificate certificate,
        string? callerThumbprint,
        CancellationToken cancellationToken);
}
