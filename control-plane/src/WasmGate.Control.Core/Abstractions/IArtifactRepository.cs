using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>Stores metadata for content-addressed release artifacts.</summary>
public interface IArtifactRepository
{
    /// <summary>Stores artifact metadata.</summary>
    /// <param name="artifact">Artifact metadata to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AddAsync(Artifact artifact, CancellationToken cancellationToken);

    /// <summary>Finds an artifact by its SHA-256 identifier.</summary>
    /// <param name="id">Content-addressed artifact identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The artifact, or null when it does not exist.</returns>
    Task<Artifact?> FindAsync(string id, CancellationToken cancellationToken);

    /// <summary>Lists all known artifacts.</summary>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Artifact metadata records.</returns>
    Task<IReadOnlyList<Artifact>> ListAsync(CancellationToken cancellationToken);
}
