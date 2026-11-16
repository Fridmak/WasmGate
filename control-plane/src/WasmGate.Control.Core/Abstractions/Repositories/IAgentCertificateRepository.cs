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

    /// <summary>Marks one certificate of an agent as revoked.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="serialNumber">Certificate serial number.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// The record is kept for audit. Revocation is not yet enforced during the
    /// TLS handshake, so a revoked certificate keeps working until it expires.
    /// </remarks>
    Task RevokeAsync(Guid agentId, string serialNumber, CancellationToken cancellationToken);
}
