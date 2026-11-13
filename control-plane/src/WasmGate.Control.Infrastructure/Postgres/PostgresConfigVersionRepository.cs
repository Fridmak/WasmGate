using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>PostgreSQL implementation of the configuration-version store.</summary>
public sealed class PostgresConfigVersionRepository(NpgsqlDataSource dataSource) : IConfigVersionRepository
{
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public Task AddAsync(ConfigVersion version, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: INSERT INTO config_versions (...) VALUES (...)");

    /// <inheritdoc />
    public Task<ConfigVersion?> FindAsync(Guid id, CancellationToken cancellationToken) =>
        throw new NotImplementedException(
            "TODO: SELECT ... FROM config_versions WHERE id = @id. " +
            "With Dapper.AOT use the SQL-string overload (not CommandDefinition).");

    /// <inheritdoc />
    public Task<ConfigVersion?> FindByHashAsync(string sha256, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM config_versions WHERE sha256 = @sha256");

    /// <inheritdoc />
    public Task<IReadOnlyList<ConfigVersion>> ListAsync(string environment, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM config_versions WHERE environment = @env ORDER BY created_at DESC");
}
