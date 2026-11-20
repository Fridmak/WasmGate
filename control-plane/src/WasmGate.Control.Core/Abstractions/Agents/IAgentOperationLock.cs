namespace WasmGate.Control.Core.Abstractions.Agents;

/// <summary>Serializes lifecycle operations for one agent.</summary>
/// <remarks>
/// The lock is process-local, matching the control plane's single-process
/// SQLite/session-registry design. It is held only around the identity check,
/// persistence and registry/database mutation; network closes happen after it
/// is released.
/// </remarks>
public interface IAgentOperationLock
{
    /// <summary>Acquires the lifecycle lock for an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="cancellationToken">Cancels waiting for the lock.</param>
    /// <returns>A handle that releases the lock on disposal.</returns>
    Task<IAsyncDisposable> AcquireAsync(Guid agentId, CancellationToken cancellationToken);
}
