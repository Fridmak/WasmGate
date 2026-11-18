using System.Data;
using Dapper;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the agent inventory.
/// </summary>
/// <remarks>
/// <c>labels</c> and <c>abi_versions</c> are JSON text; <c>status</c> is written
/// as an enum name. The upsert deliberately does not overwrite
/// <c>environment</c> or <c>registered_at</c>, matching the PostgreSQL adapter.
/// </remarks>
public sealed class SqliteAgentRepository(SqliteConnectionFactory connections) : IAgentRepository
{
    private SqliteConnectionFactory Connections { get; } = connections;

    /// <inheritdoc />
    public async Task<IReadOnlyList<Agent>> ListAsync(CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, name, environment, version, abi_versions, labels, status,
                   registered_at, last_seen_at, active_release_id, active_config_sha256
            FROM agents
            ORDER BY name, id
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var rows = await connection.QueryAsync<SqliteAgentRow>(sql);
        return rows.Select(ToAgent).ToList();
    }

    /// <inheritdoc />
    public async Task<Agent?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, name, environment, version, abi_versions, labels, status,
                   registered_at, last_seen_at, active_release_id, active_config_sha256
            FROM agents
            WHERE id = @id
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<SqliteAgentRow>(sql, new { id });
        return row is null ? null : ToAgent(row);
    }

    /// <inheritdoc />
    public async Task<Agent?> FindByNameAsync(string name, CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, name, environment, version, abi_versions, labels, status,
                   registered_at, last_seen_at, active_release_id, active_config_sha256
            FROM agents
            WHERE name = @name
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<SqliteAgentRow>(sql, new { name });
        return row is null ? null : ToAgent(row);
    }

    /// <inheritdoc />
    public async Task<bool> UpsertAsync(Agent agent, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO agents (id, name, environment, version, abi_versions, labels, status,
                                registered_at, last_seen_at, active_release_id, active_config_sha256)
            VALUES (@Id, @Name, @Environment, @Version, @AbiVersions, @Labels, @Status,
                    @RegisteredAt, @LastSeenAt, @ActiveReleaseId, @ActiveConfigSha256)
            ON CONFLICT (id) DO UPDATE SET
                name = excluded.name,
                version = excluded.version,
                abi_versions = excluded.abi_versions,
                labels = excluded.labels,
                status = excluded.status,
                last_seen_at = excluded.last_seen_at,
                active_release_id = excluded.active_release_id,
                active_config_sha256 = excluded.active_config_sha256
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await using var transaction = connection.BeginTransaction(
            IsolationLevel.Serializable,
            deferred: false);

        // Serialise with DeleteAsync: a reconnect that started before deletion
        // must observe the tombstone instead of inserting the row again. The
        // immediate transaction provides the same serialization as the
        // PostgreSQL advisory lock because SQLite has a single writer.
        var deleted = await connection.ExecuteScalarAsync<bool>(
            "SELECT EXISTS (SELECT 1 FROM deleted_agents WHERE id = @id)",
            new { id = agent.Id },
            transaction);
        if (deleted)
        {
            await transaction.CommitAsync(cancellationToken);
            return false;
        }

        var affectedRows = await connection.ExecuteAsync(sql, new
        {
            agent.Id,
            agent.Name,
            agent.Environment,
            agent.Version,
            AbiVersions = SqliteJson.SerializeAbiVersions(agent.AbiVersions),
            Labels = SqliteJson.SerializeLabels(agent.Labels),
            Status = agent.Status.ToString(),
            RegisteredAt = SqliteTimestamps.ToParameter(agent.RegisteredAt),
            LastSeenAt = SqliteTimestamps.ToParameter(agent.LastSeenAt),
            agent.ActiveReleaseId,
            agent.ActiveConfigSha256,
        }, transaction);

        await transaction.CommitAsync(cancellationToken);
        return affectedRows == 1;
    }

    /// <inheritdoc />
    public async Task SetEnvironmentAsync(
        Guid id,
        string environment,
        CancellationToken cancellationToken)
    {
        const string sql = """
            UPDATE agents
            SET environment = @environment
            WHERE id = @id
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new { id, environment });
    }

    /// <inheritdoc />
    public async Task<bool> DeleteAsync(Guid id, CancellationToken cancellationToken)
    {
        const string deleteSql = "DELETE FROM agents WHERE id = @id";
        const string tombstoneSql = """
            INSERT INTO deleted_agents (id, deleted_at)
            VALUES (@id, @deletedAt)
            ON CONFLICT (id) DO NOTHING
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await using var transaction = connection.BeginTransaction(
            IsolationLevel.Serializable,
            deferred: false);

        var affectedRows = await connection.ExecuteAsync(deleteSql, new { id }, transaction);
        if (affectedRows == 1)
        {
            await connection.ExecuteAsync(tombstoneSql, new
            {
                id,
                deletedAt = SqliteTimestamps.ToParameter(DateTimeOffset.UtcNow),
            }, transaction);
        }

        await transaction.CommitAsync(cancellationToken);
        return affectedRows == 1;
    }

    /// <inheritdoc />
    public async Task TouchAsync(
        Guid id,
        DateTimeOffset seenAt,
        string? activeReleaseId,
        string? activeConfigSha256,
        CancellationToken cancellationToken)
    {
        const string sql = """
            UPDATE agents
            SET last_seen_at = @seenAt,
                status = 'Online',
                active_release_id = CASE
                    WHEN @activeReleaseId IS NULL THEN active_release_id
                    ELSE NULLIF(@activeReleaseId, '')
                END,
                active_config_sha256 = CASE
                    WHEN @activeConfigSha256 IS NULL THEN active_config_sha256
                    ELSE NULLIF(@activeConfigSha256, '')
                END
            WHERE id = @id
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            id,
            seenAt = SqliteTimestamps.ToParameter(seenAt),
            activeReleaseId,
            activeConfigSha256,
        });
    }

    /// <inheritdoc />
    public async Task RecordEnrollmentAsync(
        Guid id,
        DateTimeOffset seenAt,
        CancellationToken cancellationToken)
    {
        const string sql = """
            UPDATE agents
            SET last_seen_at = @seenAt
            WHERE id = @id
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            id,
            seenAt = SqliteTimestamps.ToParameter(seenAt),
        });
    }

    private static Agent ToAgent(SqliteAgentRow row) => new()
    {
        Id = row.Id,
        Name = row.Name,
        Environment = row.Environment,
        Version = row.Version,
        AbiVersions = SqliteJson.ParseAbiVersions(row.AbiVersions),
        Labels = SqliteJson.ParseLabels(row.Labels),
        Status = row.Status,
        RegisteredAt = SqliteTimestamps.FromColumn(row.RegisteredAt),
        LastSeenAt = SqliteTimestamps.FromColumn(row.LastSeenAt),
        ActiveReleaseId = row.ActiveReleaseId,
        ActiveConfigSha256 = row.ActiveConfigSha256,
    };
}

/// <summary>Row shape of <c>agents</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteAgentRow
{
    public Guid Id { get; set; }

    public string Name { get; set; } = string.Empty;

    public string Environment { get; set; } = "prod";

    public string Version { get; set; } = string.Empty;

    public string AbiVersions { get; set; } = "[]";

    public string Labels { get; set; } = "{}";

    public AgentStatus Status { get; set; }

    public long RegisteredAt { get; set; }

    public long LastSeenAt { get; set; }

    public string? ActiveReleaseId { get; set; }

    public string? ActiveConfigSha256 { get; set; }
}
