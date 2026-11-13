using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>PostgreSQL implementation of the deployment store.</summary>
public sealed class PostgresDeploymentRepository(NpgsqlDataSource dataSource) : IDeploymentRepository
{
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public Task AddAsync(Deployment deployment, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: INSERT INTO deployments + deployment_waves in one transaction");

    /// <inheritdoc />
    public Task<Deployment?> FindAsync(Guid id, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM deployments JOIN deployment_waves ... WHERE id = @id");

    /// <inheritdoc />
    public Task<IReadOnlyList<Deployment>> ListActiveAsync(string environment, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM deployments WHERE environment = @env AND status IN ('pending', 'running')");

    /// <inheritdoc />
    public Task UpdateAsync(Deployment deployment, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: UPDATE deployments SET status = @status, completed_at = @completed_at WHERE id = @id");
}
