using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory artifact metadata store for the MVP.</summary>
public sealed class InMemoryArtifactRepository : IArtifactRepository
{
    private readonly ConcurrentDictionary<string, Artifact> _artifacts = new(StringComparer.OrdinalIgnoreCase);

    /// <inheritdoc />
    public Task AddAsync(Artifact artifact, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _artifacts[artifact.Id] = artifact;
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task<Artifact?> FindAsync(string id, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(_artifacts.GetValueOrDefault(id));
    }

    /// <inheritdoc />
    public Task<IReadOnlyList<Artifact>> ListAsync(CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult<IReadOnlyList<Artifact>>(
            _artifacts.Values.OrderByDescending(a => a.CreatedAt).ToList());
    }
}
