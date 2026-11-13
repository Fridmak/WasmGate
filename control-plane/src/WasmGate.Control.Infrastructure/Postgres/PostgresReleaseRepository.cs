using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>PostgreSQL implementation of the release store.</summary>
public sealed class PostgresReleaseRepository(NpgsqlDataSource dataSource) : IReleaseRepository
{
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public Task AddAsync(Release release, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: INSERT INTO releases + release_artifacts in one transaction");

    /// <inheritdoc />
    public Task<Release?> FindAsync(Guid id, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM releases JOIN release_artifacts ... WHERE id = @id");

    /// <inheritdoc />
    public Task<IReadOnlyList<Release>> ListAsync(string environment, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM releases WHERE environment = @env ORDER BY created_at DESC");

    /// <inheritdoc />
    public Task SetStatusAsync(Guid id, ReleaseStatus status, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: UPDATE releases SET status = @status WHERE id = @id");
}
