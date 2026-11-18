using System.Data;
using Dapper;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the release store.
/// </summary>
/// <remarks>
/// A release header and its artifact rows are written in one immediate
/// transaction. Dapper.AOT has no multi-mapping, so headers and artifacts are
/// read with separate queries and joined in memory.
/// </remarks>
public sealed class SqliteReleaseRepository(SqliteConnectionFactory connections) : IReleaseRepository
{
    private SqliteConnectionFactory Connections { get; } = connections;

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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await using var transaction = connection.BeginTransaction(
            IsolationLevel.Serializable,
            deferred: false);

        await connection.ExecuteAsync(headerSql, new
        {
            release.Id,
            release.Environment,
            release.ConfigVersionId,
            release.ConfigSha256,
            release.Signature,
            Status = release.Status.ToString(),
            release.CreatedBy,
            CreatedAt = SqliteTimestamps.ToParameter(release.CreatedAt),
        }, transaction);

        foreach (var artifact in release.Artifacts)
        {
            await connection.ExecuteAsync(artifactSql, new
            {
                ReleaseId = release.Id,
                artifact.ArtifactId,
                artifact.Name,
            }, transaction);
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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var header = await connection.QuerySingleOrDefaultAsync<SqliteReleaseRow>(headerSql, new { id });
        if (header is null)
        {
            return null;
        }

        var artifacts = await connection.QueryAsync<SqliteReleaseArtifactRow>(artifactSql, new { id });
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
            WHERE release_id IN @ids
            ORDER BY release_id, name
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var headers = (await connection.QueryAsync<SqliteReleaseRow>(headerSql, new { environment })).ToList();
        if (headers.Count == 0)
        {
            return [];
        }

        var ids = headers.Select(header => header.Id).ToArray();
        var artifacts = await connection.QueryAsync<SqliteReleaseArtifactRow>(artifactSql, new { ids });

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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new { id, status = status.ToString() });
    }

    private static Release ToRelease(SqliteReleaseRow header, IReadOnlyList<SqliteReleaseArtifactRow> artifacts) => new()
    {
        Id = header.Id,
        Environment = header.Environment,
        ConfigVersionId = header.ConfigVersionId,
        ConfigSha256 = header.ConfigSha256,
        Signature = header.Signature,
        Status = header.Status,
        CreatedBy = header.CreatedBy,
        CreatedAt = SqliteTimestamps.FromColumn(header.CreatedAt),
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
internal sealed class SqliteReleaseRow
{
    public Guid Id { get; set; }

    public string Environment { get; set; } = string.Empty;

    public Guid ConfigVersionId { get; set; }

    public string ConfigSha256 { get; set; } = string.Empty;

    public string? Signature { get; set; }

    public ReleaseStatus Status { get; set; }

    public string CreatedBy { get; set; } = string.Empty;

    public long CreatedAt { get; set; }
}

/// <summary>Row shape of <c>release_artifacts</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteReleaseArtifactRow
{
    public Guid ReleaseId { get; set; }

    public string ArtifactId { get; set; } = string.Empty;

    public string Name { get; set; } = string.Empty;
}
