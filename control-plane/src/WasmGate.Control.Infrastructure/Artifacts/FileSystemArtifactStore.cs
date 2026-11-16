using System.Buffers;
using System.Security.Cryptography;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Infrastructure.Artifacts;

/// <summary>
/// Local content-addressed artifact store for one control-plane instance.
/// </summary>
/// <remarks>
/// The layout is <c>{ArtifactRoot}/sha256/{aa}/{bb}/{sha256}</c>, so the same
/// bytes are always stored once. Uploads go to a temporary file and are moved
/// into place, which also keeps concurrent uploads of identical content safe.
/// </remarks>
public sealed class FileSystemArtifactStore(ControlPlaneOptions options) : IArtifactStore
{
    private string Root => Path.GetFullPath(options.ArtifactRoot);

    /// <inheritdoc />
    public Task<Stream> OpenReadAsync(string artifactId, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        var path = PathFor(artifactId);
        return Task.FromResult<Stream>(new FileStream(
            path,
            FileMode.Open,
            FileAccess.Read,
            FileShare.Read,
            bufferSize: 64 * 1024,
            useAsync: true));
    }

    /// <inheritdoc />
    public async Task<Artifact> PutAsync(
        Stream content,
        ArtifactKind kind,
        string? mediaType,
        string createdBy,
        CancellationToken cancellationToken)
    {
        ArgumentNullException.ThrowIfNull(content);

        Directory.CreateDirectory(Root);
        var tempPath = Path.Combine(Root, $".upload-{Guid.NewGuid():N}");
        using var hash = IncrementalHash.CreateHash(HashAlgorithmName.SHA256);
        var buffer = ArrayPool<byte>.Shared.Rent(64 * 1024);
        long size = 0;

        try
        {
            await using (var output = new FileStream(
                             tempPath,
                             FileMode.CreateNew,
                             FileAccess.Write,
                             FileShare.None,
                             bufferSize: 64 * 1024,
                             useAsync: true))
            {
                int read;
                while ((read = await content.ReadAsync(buffer.AsMemory(), cancellationToken)) > 0)
                {
                    hash.AppendData(buffer, 0, read);
                    await output.WriteAsync(buffer.AsMemory(0, read), cancellationToken);
                    size += read;
                }
            }

            var id = Convert.ToHexString(hash.GetHashAndReset()).ToLowerInvariant();
            var destination = PathFor(id);
            Directory.CreateDirectory(Path.GetDirectoryName(destination)!);

            // Content-addressed files are immutable, so overwriting an existing
            // file is safe. Overwrite also closes the race between the
            // File.Exists check and the move when two uploads finish at once.
            File.Move(tempPath, destination, overwrite: true);

            return new Artifact
            {
                Id = id,
                Kind = kind,
                Size = size,
                MediaType = mediaType,
                CreatedBy = createdBy,
            };
        }
        finally
        {
            ArrayPool<byte>.Shared.Return(buffer);
            if (File.Exists(tempPath))
            {
                File.Delete(tempPath);
            }
        }
    }

    /// <inheritdoc />
    public Task<bool> ExistsAsync(string artifactId, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(File.Exists(PathFor(artifactId)));
    }

    private string PathFor(string artifactId)
    {
        if (artifactId.Length != 64 || !artifactId.All(Uri.IsHexDigit))
        {
            throw new ArgumentException("Artifact ID must be a 64-character SHA-256 hex string.", nameof(artifactId));
        }

        var id = artifactId.ToLowerInvariant();
        return Path.Combine(Root, "sha256", id[..2], id[2..4], id);
    }
}
