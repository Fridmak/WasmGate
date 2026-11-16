using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>Stores metadata for content-addressed release artifacts.</summary>
/// <remarks>Artifact bytes live in <c>IArtifactStore</c>; this contract covers the database rows.</remarks>
public interface IArtifactRepository
{
    /// <summary>Stores artifact metadata, ignoring an identifier that already exists.</summary>
    /// <param name="artifact">Artifact metadata to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Uploading the same content twice is normal: the identifier is the SHA-256
    /// of the bytes, and the first stored metadata wins.
    /// </remarks>
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
