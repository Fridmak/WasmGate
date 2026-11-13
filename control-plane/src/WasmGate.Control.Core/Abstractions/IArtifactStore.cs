using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>
/// Content-addressed binary storage for release artifacts. Implementations may
/// use a local directory, S3 or MinIO; the contract is SHA-256 addressed.
/// </summary>
public interface IArtifactStore
{
    /// <summary>Opens an artifact for reading.</summary>
    /// <param name="artifactId">Content-addressed artifact identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>A readable stream owned by the caller.</returns>
    Task<Stream> OpenReadAsync(string artifactId, CancellationToken cancellationToken);

    /// <summary>Stores an artifact and returns its content-addressed metadata.</summary>
    /// <param name="content">Artifact bytes. The implementation must not dispose the stream.</param>
    /// <param name="kind">Semantic artifact kind.</param>
    /// <param name="mediaType">Optional MIME type.</param>
    /// <param name="createdBy">Identity of the uploading principal.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Metadata of the stored artifact.</returns>
    Task<Artifact> PutAsync(
        Stream content,
        ArtifactKind kind,
        string? mediaType,
        string createdBy,
        CancellationToken cancellationToken);

    /// <summary>Checks whether an artifact exists.</summary>
    /// <param name="artifactId">Content-addressed artifact identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>True when the artifact is present in the store.</returns>
    Task<bool> ExistsAsync(string artifactId, CancellationToken cancellationToken);
}
