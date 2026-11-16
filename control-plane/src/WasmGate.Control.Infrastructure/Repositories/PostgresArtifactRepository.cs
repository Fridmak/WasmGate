using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Repositories.Convert;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the artifact metadata store.
/// </summary>
/// <remarks>
/// Artifact bytes live in <see cref="IArtifactStore"/>; this table keeps only
/// metadata. Identifiers are content hashes, so storing the same artifact twice
/// is normal and does not fail.
/// </remarks>
public sealed class PostgresArtifactRepository(NpgsqlDataSource dataSource) : IArtifactRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task AddAsync(Artifact artifact, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO artifacts (id, kind, size, media_type, signature, created_by, created_at)
            VALUES (@Id, @Kind, @Size, @MediaType, @Signature, @CreatedBy, @CreatedAt)
            ON CONFLICT (id) DO NOTHING
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            artifact.Id,
            Kind = artifact.Kind.ToString(),
            artifact.Size,
            artifact.MediaType,
            artifact.Signature,
            artifact.CreatedBy,
            CreatedAt = PostgresTimestamps.ToParameter(artifact.CreatedAt),
        });
    }

    /// <inheritdoc />
    public async Task<Artifact?> FindAsync(string id, CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, kind, size, media_type, signature, created_by, created_at
            FROM artifacts
            WHERE id = @id
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<ArtifactRow>(sql, new { id });
        return row is null ? null : ToArtifact(row);
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<Artifact>> ListAsync(CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, kind, size, media_type, signature, created_by, created_at
            FROM artifacts
            ORDER BY created_at DESC, id DESC
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var rows = await connection.QueryAsync<ArtifactRow>(sql);
        return rows.Select(ToArtifact).ToList();
    }

    /// <summary>Maps one row of <c>artifacts</c> to domain metadata.</summary>
    /// <param name="row">Row read from the database.</param>
    /// <returns>The mapped artifact.</returns>
    private static Artifact ToArtifact(ArtifactRow row) => new()
    {
        Id = row.Id,
        Kind = row.Kind,
        Size = row.Size,
        MediaType = row.MediaType,
        Signature = row.Signature,
        CreatedBy = row.CreatedBy,
        CreatedAt = PostgresTimestamps.FromColumn(row.CreatedAt),
    };
}

/// <summary>Row shape of <c>artifacts</c> as read by Dapper.AOT.</summary>
/// <remarks><c>created_at</c> arrives as a UTC <see cref="DateTime"/>.</remarks>
internal sealed class ArtifactRow
{
    public string Id { get; set; } = string.Empty;

    public ArtifactKind Kind { get; set; }

    public long Size { get; set; }

    public string? MediaType { get; set; }

    public string? Signature { get; set; }

    public string? CreatedBy { get; set; }

    public DateTime CreatedAt { get; set; }
}
