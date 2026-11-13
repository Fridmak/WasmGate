using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>Stores certificates issued to data planes.</summary>
public interface IAgentCertificateRepository
{
    /// <summary>Stores an issued certificate.</summary>
    /// <param name="certificate">Certificate to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken);

    /// <summary>Lists active certificates for an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Certificates not yet revoked.</returns>
    Task<IReadOnlyList<AgentCertificate>> ListActiveByAgentAsync(
        Guid agentId,
        CancellationToken cancellationToken);

    /// <summary>Revokes one certificate of an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="serialNumber">Certificate serial number.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task RevokeAsync(Guid agentId, string serialNumber, CancellationToken cancellationToken);
}
