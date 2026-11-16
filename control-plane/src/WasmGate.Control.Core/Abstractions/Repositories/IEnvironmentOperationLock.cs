namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>
/// Serialises operations that must not race with environment deletion.
/// </summary>
/// <remarks>
/// An implementation holds a database-level advisory lock for the duration of
/// the returned handle, so several control-plane processes cooperate. The lock
/// is keyed by the normalised environment name.
/// </remarks>
public interface IEnvironmentOperationLock
{
    /// <summary>Acquires the lock for an environment.</summary>
    /// <param name="environment">Normalised environment name.</param>
    /// <param name="cancellationToken">Cancels the wait.</param>
    /// <returns>A handle that releases the lock when disposed.</returns>
    Task<IAsyncDisposable> AcquireAsync(string environment, CancellationToken cancellationToken);
}
