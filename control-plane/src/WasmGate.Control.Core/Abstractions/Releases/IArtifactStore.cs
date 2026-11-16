using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Core.Abstractions.Releases;

/// <summary>
/// Binary storage for release artifacts, addressed by the SHA-256 of their
/// content. Storing the same bytes twice yields the same identifier, so an
/// artifact is never duplicated.
/// </summary>
/// <remarks>
/// Only the filesystem implementation exists today; the contract stays
/// storage-agnostic, so the bytes may move to a remote store without touching
/// callers.
/// </remarks>
public interface IArtifactStore
{
    /// <summary>Opens an artifact for reading.</summary>
    /// <param name="artifactId">SHA-256 hex identifier of the artifact.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>A readable stream owned by the caller.</returns>
    /// <exception cref="ArgumentException">The identifier is not a SHA-256 hex string.</exception>
    /// <exception cref="FileNotFoundException">The artifact does not exist.</exception>
    Task<Stream> OpenReadAsync(string artifactId, CancellationToken cancellationToken);

    /// <summary>Stores an artifact and returns its content-addressed metadata.</summary>
    /// <param name="content">Artifact bytes, read to the end and not disposed.</param>
    /// <param name="kind">Semantic artifact kind.</param>
    /// <param name="mediaType">Optional MIME type.</param>
    /// <param name="createdBy">Identity of the uploading principal.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Metadata whose <c>Id</c> is the SHA-256 of the bytes.</returns>
    Task<Artifact> PutAsync(
        Stream content,
        ArtifactKind kind,
        string? mediaType,
        string createdBy,
        CancellationToken cancellationToken);

    /// <summary>Checks whether an artifact exists.</summary>
    /// <param name="artifactId">SHA-256 hex identifier of the artifact.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>True when the artifact is present in the store.</returns>
    Task<bool> ExistsAsync(string artifactId, CancellationToken cancellationToken);
}
