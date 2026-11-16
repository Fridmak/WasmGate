using System.Text.Json;
using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Repositories.Convert;
using WasmGate.Control.Infrastructure.Repositories.Serialization;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the agent inventory.
/// </summary>
/// <remarks>
/// <para>
/// <c>labels</c> is a <c>jsonb</c> column; it is read as text and parsed through
/// <see cref="PostgresJsonContext"/>, because the domain entity exposes an
/// <see cref="IReadOnlyDictionary{TKey,TValue}"/>.
/// </para>
/// <para>
/// <c>status</c> is written with <see cref="Enum.ToString()"/>; Dapper.AOT parses
/// it back into <see cref="AgentStatus"/> case-insensitively.
/// </para>
/// </remarks>
public sealed class PostgresAgentRepository(NpgsqlDataSource dataSource) : IAgentRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task<IReadOnlyList<Agent>> ListAsync(CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, name, version, abi_versions, labels::text AS labels, status,
                   registered_at, last_seen_at, active_release_id, active_config_sha256
            FROM agents
            ORDER BY name, id
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var rows = await connection.QueryAsync<AgentRow>(sql);
        return rows.Select(ToAgent).ToList();
    }

    /// <inheritdoc />
    public async Task<Agent?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, name, version, abi_versions, labels::text AS labels, status,
                   registered_at, last_seen_at, active_release_id, active_config_sha256
            FROM agents
            WHERE id = @id
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<AgentRow>(sql, new { id });
        return row is null ? null : ToAgent(row);
    }

    /// <inheritdoc />
    public async Task<Agent?> FindByNameAsync(string name, CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, name, version, abi_versions, labels::text AS labels, status,
                   registered_at, last_seen_at, active_release_id, active_config_sha256
            FROM agents
            WHERE name = @name
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<AgentRow>(sql, new { name });
        return row is null ? null : ToAgent(row);
    }

    /// <inheritdoc />
    public async Task UpsertAsync(Agent agent, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO agents (id, name, version, abi_versions, labels, status,
                                registered_at, last_seen_at, active_release_id, active_config_sha256)
            VALUES (@Id, @Name, @Version, @AbiVersions, @Labels::jsonb, @Status,
                    @RegisteredAt, @LastSeenAt, @ActiveReleaseId, @ActiveConfigSha256)
            ON CONFLICT (id) DO UPDATE SET
                name = EXCLUDED.name,
                version = EXCLUDED.version,
                abi_versions = EXCLUDED.abi_versions,
                labels = EXCLUDED.labels,
                status = EXCLUDED.status,
                last_seen_at = EXCLUDED.last_seen_at,
                active_release_id = EXCLUDED.active_release_id,
                active_config_sha256 = EXCLUDED.active_config_sha256
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            agent.Id,
            agent.Name,
            agent.Version,
            AbiVersions = agent.AbiVersions.ToArray(),
            Labels = SerializeLabels(agent.Labels),
            Status = agent.Status.ToString(),
            RegisteredAt = PostgresTimestamps.ToParameter(agent.RegisteredAt),
            LastSeenAt = PostgresTimestamps.ToParameter(agent.LastSeenAt),
            agent.ActiveReleaseId,
            agent.ActiveConfigSha256,
        });
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
                    WHEN @activeReleaseId::text IS NULL THEN active_release_id
                    ELSE NULLIF(@activeReleaseId::text, '')
                END,
                active_config_sha256 = CASE
                    WHEN @activeConfigSha256::text IS NULL THEN active_config_sha256
                    ELSE NULLIF(@activeConfigSha256::text, '')
                END
            WHERE id = @id
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            id,
            seenAt = PostgresTimestamps.ToParameter(seenAt),
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

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            id,
            seenAt = PostgresTimestamps.ToParameter(seenAt),
        });
    }

    /// <summary>Maps one row of <c>agents</c> to a domain agent.</summary>
    /// <param name="row">Row read from the database.</param>
    /// <returns>The mapped agent.</returns>
    private static Agent ToAgent(AgentRow row) => new()
    {
        Id = row.Id,
        Name = row.Name,
        Version = row.Version,
        AbiVersions = row.AbiVersions,
        Labels = ParseLabels(row.Labels),
        Status = row.Status,
        RegisteredAt = PostgresTimestamps.FromColumn(row.RegisteredAt),
        LastSeenAt = PostgresTimestamps.FromColumn(row.LastSeenAt),
        ActiveReleaseId = row.ActiveReleaseId,
        ActiveConfigSha256 = row.ActiveConfigSha256,
    };

    /// <summary>Serializes labels for the <c>jsonb</c> column.</summary>
    /// <param name="labels">Labels reported by the agent.</param>
    /// <returns>A JSON object with the same entries.</returns>
    private static string SerializeLabels(IReadOnlyDictionary<string, string> labels) =>
        JsonSerializer.Serialize(
            new Dictionary<string, string>(labels, StringComparer.Ordinal),
            PostgresJsonContext.Default.DictionaryStringString);

    /// <summary>Parses labels read from the <c>jsonb</c> column.</summary>
    /// <param name="json">JSON object stored in the column.</param>
    /// <returns>A non-null, case-sensitive label dictionary.</returns>
    private static IReadOnlyDictionary<string, string> ParseLabels(string json) =>
        JsonSerializer.Deserialize(json, PostgresJsonContext.Default.DictionaryStringString)
        ?? new Dictionary<string, string>(StringComparer.Ordinal);
}

/// <summary>Row shape of <c>agents</c> as read by Dapper.AOT.</summary>
/// <remarks>
/// <c>labels</c> arrives as JSON text and <c>status</c> as an enum name; timestamps
/// arrive as UTC <see cref="DateTime"/>.
/// </remarks>
internal sealed class AgentRow
{
    public Guid Id { get; set; }

    public string Name { get; set; } = string.Empty;

    public string Version { get; set; } = string.Empty;

    public string[] AbiVersions { get; set; } = [];

    public string Labels { get; set; } = "{}";

    public AgentStatus Status { get; set; }

    public DateTime RegisteredAt { get; set; }

    public DateTime LastSeenAt { get; set; }

    public string? ActiveReleaseId { get; set; }

    public string? ActiveConfigSha256 { get; set; }
}
