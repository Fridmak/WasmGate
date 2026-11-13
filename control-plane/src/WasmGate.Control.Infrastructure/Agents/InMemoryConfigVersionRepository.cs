using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory configuration-version store for the MVP.</summary>
public sealed class InMemoryConfigVersionRepository : IConfigVersionRepository
{
    private readonly ConcurrentDictionary<Guid, ConfigVersion> _versions = new();

    /// <inheritdoc />
    public Task AddAsync(ConfigVersion version, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _versions[version.Id] = version;
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task<ConfigVersion?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(_versions.GetValueOrDefault(id));
    }

    /// <inheritdoc />
    public Task<ConfigVersion?> FindByHashAsync(string sha256, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(_versions.Values.FirstOrDefault(v =>
            string.Equals(v.Sha256, sha256, StringComparison.OrdinalIgnoreCase)));
    }

    /// <inheritdoc />
    public Task<IReadOnlyList<ConfigVersion>> ListAsync(string environment, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult<IReadOnlyList<ConfigVersion>>(
            _versions.Values
                .Where(v => string.Equals(v.Environment, environment, StringComparison.OrdinalIgnoreCase))
                .OrderByDescending(v => v.CreatedAt)
                .ToList());
    }
}
