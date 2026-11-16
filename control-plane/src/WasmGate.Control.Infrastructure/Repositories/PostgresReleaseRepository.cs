using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Repositories.Convert;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the release store.
/// </summary>
/// <remarks>
/// <para>
/// A release spans two tables: <c>releases</c> for the header and
/// <c>release_artifacts</c> for the install paths. Both are written in one
/// transaction, because a release without its artifacts is unusable.
/// </para>
/// <para>
/// Dapper.AOT has no multi-mapping, so headers and artifact rows are read with
/// separate queries and joined in memory.
/// </para>
/// </remarks>
public sealed class PostgresReleaseRepository(NpgsqlDataSource dataSource) : IReleaseRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task AddAsync(Release release, CancellationToken cancellationToken)
    {
        const string headerSql = """
            INSERT INTO releases (id, environment, config_version_id, config_sha256,
                                  signature, status, created_by, created_at)
            VALUES (@Id, @Environment, @ConfigVersionId, @ConfigSha256,
                    @Signature, @Status, @CreatedBy, @CreatedAt)
            """;

        const string artifactSql = """
            INSERT INTO release_artifacts (release_id, artifact_id, name)
            VALUES (@ReleaseId, @ArtifactId, @Name)
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await using var transaction = await connection.BeginTransactionAsync(cancellationToken);

        await connection.ExecuteAsync(headerSql, new
        {
            release.Id,
            release.Environment,
            release.ConfigVersionId,
            release.ConfigSha256,
            release.Signature,
            Status = release.Status.ToString(),
            release.CreatedBy,
            CreatedAt = PostgresTimestamps.ToParameter(release.CreatedAt),
        }, transaction: transaction);

        foreach (var artifact in release.Artifacts)
        {
            await connection.ExecuteAsync(artifactSql, new
            {
                ReleaseId = release.Id,
                artifact.ArtifactId,
                artifact.Name,
            }, transaction: transaction);
        }

        await transaction.CommitAsync(cancellationToken);
    }

    /// <inheritdoc />
    public async Task<Release?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, config_version_id, config_sha256, signature,
                   status, created_by, created_at
            FROM releases
            WHERE id = @id
            """;

        const string artifactSql = """
            SELECT release_id, artifact_id, name
            FROM release_artifacts
            WHERE release_id = @id
            ORDER BY name
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var header = await connection.QuerySingleOrDefaultAsync<ReleaseRow>(headerSql, new { id });
        if (header is null)
        {
            return null;
        }

        var artifacts = await connection.QueryAsync<ReleaseArtifactRow>(artifactSql, new { id });
        return ToRelease(header, artifacts.ToList());
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<Release>> ListAsync(string environment, CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, config_version_id, config_sha256, signature,
                   status, created_by, created_at
            FROM releases
            WHERE environment = @environment
            ORDER BY created_at DESC, id DESC
            """;

        const string artifactSql = """
            SELECT release_id, artifact_id, name
            FROM release_artifacts
            WHERE release_id = ANY(@ids)
            ORDER BY release_id, name
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var headers = (await connection.QueryAsync<ReleaseRow>(headerSql, new { environment })).ToList();
        if (headers.Count == 0)
        {
            return [];
        }

        var ids = headers.Select(header => header.Id).ToArray();
        var artifacts = await connection.QueryAsync<ReleaseArtifactRow>(artifactSql, new { ids });

        var artifactsByRelease = artifacts
            .GroupBy(artifact => artifact.ReleaseId)
            .ToDictionary(group => group.Key, group => group.ToList());

        return headers
            .Select(header => ToRelease(
                header,
                artifactsByRelease.GetValueOrDefault(header.Id) ?? []))
            .ToList();
    }

    /// <inheritdoc />
    public async Task SetStatusAsync(Guid id, ReleaseStatus status, CancellationToken cancellationToken)
    {
        const string sql = """
            UPDATE releases
            SET status = @status
            WHERE id = @id
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new { id, status = status.ToString() });
    }

    /// <summary>Combines a release header with its artifact rows.</summary>
    /// <param name="header">Row read from <c>releases</c>.</param>
    /// <param name="artifacts">Rows read from <c>release_artifacts</c>.</param>
    /// <returns>The mapped release.</returns>
    private static Release ToRelease(ReleaseRow header, IReadOnlyList<ReleaseArtifactRow> artifacts) => new()
    {
        Id = header.Id,
        Environment = header.Environment,
        ConfigVersionId = header.ConfigVersionId,
        ConfigSha256 = header.ConfigSha256,
        Signature = header.Signature,
        Status = header.Status,
        CreatedBy = header.CreatedBy,
        CreatedAt = PostgresTimestamps.FromColumn(header.CreatedAt),
        Artifacts = artifacts
            .Select(artifact => new ReleaseArtifact
            {
                ArtifactId = artifact.ArtifactId,
                Name = artifact.Name,
            })
            .ToList(),
    };
}

/// <summary>Row shape of <c>releases</c> as read by Dapper.AOT.</summary>
/// <remarks><c>status</c> arrives as an enum name; <c>created_at</c> as a UTC <see cref="DateTime"/>.</remarks>
internal sealed class ReleaseRow
{
    public Guid Id { get; set; }

    public string Environment { get; set; } = string.Empty;

    public Guid ConfigVersionId { get; set; }

    public string ConfigSha256 { get; set; } = string.Empty;

    public string? Signature { get; set; }

    public ReleaseStatus Status { get; set; }

    public string CreatedBy { get; set; } = string.Empty;

    public DateTime CreatedAt { get; set; }
}

/// <summary>Row shape of <c>release_artifacts</c> as read by Dapper.AOT.</summary>
internal sealed class ReleaseArtifactRow
{
    public Guid ReleaseId { get; set; }

    public string ArtifactId { get; set; } = string.Empty;

    public string Name { get; set; } = string.Empty;
}
