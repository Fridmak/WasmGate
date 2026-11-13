using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>Persists the fleet inventory of data-plane agents.</summary>
public interface IAgentRepository
{
    /// <summary>Returns all registered agents.</summary>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>All agents known to the control plane.</returns>
    Task<IReadOnlyList<Agent>> ListAsync(CancellationToken cancellationToken);

    /// <summary>Finds an agent by its identifier.</summary>
    /// <param name="id">Agent identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The agent, or null when it does not exist.</returns>
    Task<Agent?> FindAsync(Guid id, CancellationToken cancellationToken);

    /// <summary>Finds an agent by its operator-visible name.</summary>
    /// <param name="name">Unique agent name.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The agent, or null when it does not exist.</returns>
    Task<Agent?> FindByNameAsync(string name, CancellationToken cancellationToken);

    /// <summary>Inserts a new agent or updates its static identity fields.</summary>
    /// <param name="agent">Agent to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task UpsertAsync(Agent agent, CancellationToken cancellationToken);

    /// <summary>Updates the liveness and active-release fields of an agent.</summary>
    /// <param name="id">Agent identifier.</param>
    /// <param name="seenAt">Time of the last heartbeat or control message.</param>
    /// <param name="activeReleaseId">Release currently applied by the agent, if known.</param>
    /// <param name="activeConfigSha256">SHA-256 of the active configuration, if known.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task TouchAsync(
        Guid id,
        DateTimeOffset seenAt,
        string? activeReleaseId,
        string? activeConfigSha256,
        CancellationToken cancellationToken);
}
