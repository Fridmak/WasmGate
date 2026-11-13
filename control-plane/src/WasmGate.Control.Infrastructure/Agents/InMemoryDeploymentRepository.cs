using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory deployment store for the MVP.</summary>
public sealed class InMemoryDeploymentRepository : IDeploymentRepository
{
    private readonly ConcurrentDictionary<Guid, Deployment> _deployments = new();

    /// <inheritdoc />
    public Task AddAsync(Deployment deployment, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _deployments[deployment.Id] = deployment;
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task<Deployment?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(_deployments.GetValueOrDefault(id));
    }

    /// <inheritdoc />
    public Task<IReadOnlyList<Deployment>> ListActiveAsync(string environment, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult<IReadOnlyList<Deployment>>(
            _deployments.Values
                .Where(d => string.Equals(d.Environment, environment, StringComparison.OrdinalIgnoreCase) &&
                            d.Status is DeploymentStatus.Pending or DeploymentStatus.Running)
                .OrderByDescending(d => d.CreatedAt)
                .ToList());
    }

    /// <inheritdoc />
    public Task UpdateAsync(Deployment deployment, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _deployments[deployment.Id] = deployment;
        return Task.CompletedTask;
    }
}
