using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Repositories.Convert;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the configuration-version store.
/// </summary>
/// <remarks>
/// <para>
/// A configuration version is immutable. <see cref="ConfigVersion.CreatedAt"/>
/// is set by the service and written explicitly.
/// </para>
/// <para>
/// The unique index is <c>(environment, sha256)</c>, so the same YAML may exist
/// in several environments. A duplicate upload is ignored instead of failing:
/// the caller reads the stored row with <see cref="FindByHashAsync"/>.
/// </para>
/// </remarks>
public sealed class PostgresConfigVersionRepository(NpgsqlDataSource dataSource) : IConfigVersionRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task AddAsync(ConfigVersion version, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO config_versions (id, environment, sha256, yaml, message, created_by, created_at)
            VALUES (@Id, @Environment, @Sha256, @Yaml, @Message, @CreatedBy, @CreatedAt)
            ON CONFLICT (environment, sha256) DO NOTHING
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            version.Id,
            version.Environment,
            version.Sha256,
            version.Yaml,
            version.Message,
            version.CreatedBy,
            CreatedAt = PostgresTimestamps.ToParameter(version.CreatedAt),
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

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<ConfigVersionRow>(sql, new { id });
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

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<ConfigVersionRow>(
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

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var rows = await connection.QueryAsync<ConfigVersionRow>(sql, new { environment });
        return rows.Select(ToConfigVersion).ToList();
    }

    /// <summary>Maps one row of <c>config_versions</c> to a domain version.</summary>
    /// <param name="row">Row read from the database.</param>
    /// <returns>The mapped configuration version.</returns>
    private static ConfigVersion ToConfigVersion(ConfigVersionRow row) => new()
    {
        Id = row.Id,
        Environment = row.Environment,
        Sha256 = row.Sha256,
        Yaml = row.Yaml,
        Message = row.Message,
        CreatedBy = row.CreatedBy,
        CreatedAt = PostgresTimestamps.FromColumn(row.CreatedAt),
    };
}

/// <summary>Row shape of <c>config_versions</c> as read by Dapper.AOT.</summary>
/// <remarks><c>created_at</c> arrives as a UTC <see cref="DateTime"/>.</remarks>
internal sealed class ConfigVersionRow
{
    public Guid Id { get; set; }

    public string Environment { get; set; } = string.Empty;

    public string Sha256 { get; set; } = string.Empty;

    public string Yaml { get; set; } = string.Empty;

    public string? Message { get; set; }

    public string CreatedBy { get; set; } = string.Empty;

    public DateTime CreatedAt { get; set; }
}
