using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>Stores immutable releases and their lifecycle state.</summary>
public interface IReleaseRepository
{
    /// <summary>Stores a new release together with its artifact references.</summary>
    /// <param name="release">Release to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>The release and its artifact references are written in one transaction.</remarks>
    Task AddAsync(Release release, CancellationToken cancellationToken);

    /// <summary>Finds a release by its identifier.</summary>
    /// <param name="id">Release identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The release with its artifacts, or null when it does not exist.</returns>
    Task<Release?> FindAsync(Guid id, CancellationToken cancellationToken);

    /// <summary>Lists releases for an environment, newest first.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Releases belonging to the environment.</returns>
    /// <remarks>Rollback relies on this order: the next element after the current release is the previous one.</remarks>
    Task<IReadOnlyList<Release>> ListAsync(string environment, CancellationToken cancellationToken);

    /// <summary>Changes the lifecycle state of a release.</summary>
    /// <param name="id">Release identifier.</param>
    /// <param name="status">New release status; a Revoked release cannot be deployed.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task SetStatusAsync(Guid id, ReleaseStatus status, CancellationToken cancellationToken);
}
