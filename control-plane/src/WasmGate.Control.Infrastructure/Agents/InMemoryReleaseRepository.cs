using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory release store for the MVP.</summary>
public sealed class InMemoryReleaseRepository : IReleaseRepository
{
    private readonly ConcurrentDictionary<Guid, Release> _releases = new();

    /// <inheritdoc />
    public Task AddAsync(Release release, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _releases[release.Id] = release;
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task<Release?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(_releases.GetValueOrDefault(id));
    }

    /// <inheritdoc />
    public Task<IReadOnlyList<Release>> ListAsync(string environment, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult<IReadOnlyList<Release>>(
            _releases.Values
                .Where(r => string.Equals(r.Environment, environment, StringComparison.OrdinalIgnoreCase))
                .OrderByDescending(r => r.CreatedAt)
                .ToList());
    }

    /// <inheritdoc />
    public Task SetStatusAsync(Guid id, ReleaseStatus status, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        if (_releases.TryGetValue(id, out var release))
        {
            release.Status = status;
        }

        return Task.CompletedTask;
    }
}
