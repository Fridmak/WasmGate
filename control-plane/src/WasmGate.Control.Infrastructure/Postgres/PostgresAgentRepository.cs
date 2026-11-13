using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>
/// PostgreSQL implementation of the agent inventory. SQL is hand-written
/// because EF Core and Dapper are not Native AOT compatible; Dapper.AOT is the
/// intended generator for the future call sites.
/// </summary>
public sealed class PostgresAgentRepository(NpgsqlDataSource dataSource) : IAgentRepository
{
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public Task<IReadOnlyList<Agent>> ListAsync(CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT id, name, version, abi_versions, labels, status, registered_at, last_seen_at, active_release_id, active_config_sha256 FROM agents ORDER BY name");

    /// <inheritdoc />
    public Task<Agent?> FindAsync(Guid id, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM agents WHERE id = @id");

    /// <inheritdoc />
    public Task<Agent?> FindByNameAsync(string name, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM agents WHERE name = @name");

    /// <inheritdoc />
    public Task UpsertAsync(Agent agent, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: INSERT ... ON CONFLICT (id) DO UPDATE ...");

    /// <inheritdoc />
    public Task TouchAsync(
        Guid id,
        DateTimeOffset seenAt,
        string? activeReleaseId,
        string? activeConfigSha256,
        CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: UPDATE agents SET last_seen_at = @seen_at, active_release_id = @release, active_config_sha256 = @hash, status = 'online' WHERE id = @id");
}
