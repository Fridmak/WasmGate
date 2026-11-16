using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>
/// Durable inventory of data-plane agents: identity, labels, status and the
/// release they report. Open connections are not stored here.
/// </summary>
public interface IAgentRepository
{
    /// <summary>Returns all registered agents ordered by name.</summary>
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

    /// <summary>
    /// Inserts a new agent or overwrites the identity and reported runtime fields
    /// of an existing one.
    /// </summary>
    /// <param name="agent">Agent to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Writing over existing fields makes this the single mutation path for the
    /// current state of an agent. The registration time of an existing agent is
    /// kept.
    /// </remarks>
    Task UpsertAsync(Agent agent, CancellationToken cancellationToken);

    /// <summary>Records contact with an agent and marks it Online.</summary>
    /// <param name="id">Agent identifier.</param>
    /// <param name="seenAt">Time of the last heartbeat or control message.</param>
    /// <param name="activeReleaseId">
    /// Release currently applied by the agent. Null keeps the stored value; an
    /// empty string clears it.
    /// </param>
    /// <param name="activeConfigSha256">
    /// SHA-256 of the active configuration. Null keeps the stored value; an
    /// empty string clears it.
    /// </param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task TouchAsync(
        Guid id,
        DateTimeOffset seenAt,
        string? activeReleaseId,
        string? activeConfigSha256,
        CancellationToken cancellationToken);
}
