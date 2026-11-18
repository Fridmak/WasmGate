using Dapper;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the configuration-version store.
/// </summary>
/// <remarks>
/// The unique constraint is <c>(environment, sha256)</c>, so the same YAML may
/// exist in several environments. A duplicate upload is ignored instead of
/// failing; the caller reads the stored row with <see cref="FindByHashAsync"/>.
/// </remarks>
public sealed class SqliteConfigVersionRepository(SqliteConnectionFactory connections) : IConfigVersionRepository
{
    private SqliteConnectionFactory Connections { get; } = connections;

    /// <inheritdoc />
    public async Task AddAsync(ConfigVersion version, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO config_versions (id, environment, sha256, yaml, message, created_by, created_at)
            VALUES (@Id, @Environment, @Sha256, @Yaml, @Message, @CreatedBy, @CreatedAt)
            ON CONFLICT (environment, sha256) DO NOTHING
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            version.Id,
            version.Environment,
            version.Sha256,
            version.Yaml,
            version.Message,
            version.CreatedBy,
            CreatedAt = SqliteTimestamps.ToParameter(version.CreatedAt),
        });
    }

    /// <inheritdoc />
    public async Task<ConfigVersion?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, environment, sha256, yaml, message, created_by, created_at
            FROM config_versions
            WHERE id = @id
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<SqliteConfigVersionRow>(sql, new { id });
        return row is null ? null : ToConfigVersion(row);
    }

    /// <inheritdoc />
    public async Task<ConfigVersion?> FindByHashAsync(
        string environment,
        string sha256,
        CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, environment, sha256, yaml, message, created_by, created_at
            FROM config_versions
            WHERE environment = @environment AND sha256 = @sha256
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<SqliteConfigVersionRow>(
            sql,
            new { environment, sha256 });
        return row is null ? null : ToConfigVersion(row);
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<ConfigVersion>> ListAsync(
        string environment,
        CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, environment, sha256, yaml, message, created_by, created_at
            FROM config_versions
            WHERE environment = @environment
            ORDER BY created_at DESC, id DESC
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var rows = await connection.QueryAsync<SqliteConfigVersionRow>(sql, new { environment });
        return rows.Select(ToConfigVersion).ToList();
    }

    private static ConfigVersion ToConfigVersion(SqliteConfigVersionRow row) => new()
    {
        Id = row.Id,
        Environment = row.Environment,
        Sha256 = row.Sha256,
        Yaml = row.Yaml,
        Message = row.Message,
        CreatedBy = row.CreatedBy,
        CreatedAt = SqliteTimestamps.FromColumn(row.CreatedAt),
    };
}

/// <summary>Row shape of <c>config_versions</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteConfigVersionRow
{
    public Guid Id { get; set; }

    public string Environment { get; set; } = string.Empty;

    public string Sha256 { get; set; } = string.Empty;

    public string Yaml { get; set; } = string.Empty;

    public string? Message { get; set; }

    public string CreatedBy { get; set; } = string.Empty;

    public long CreatedAt { get; set; }
}
