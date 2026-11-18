using Dapper;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the enrollment-token store.
/// </summary>
/// <remarks>
/// Token usage is reserved with a single conditional UPDATE, so two concurrent
/// enrollments cannot exceed <see cref="EnrollmentToken.MaxUses"/> even without
/// an explicit transaction.
/// </remarks>
public sealed class SqliteEnrollmentTokenRepository(SqliteConnectionFactory connections) : IEnrollmentTokenRepository
{
    private SqliteConnectionFactory Connections { get; } = connections;

    /// <inheritdoc />
    public async Task AddAsync(EnrollmentToken token, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO enrollment_tokens (id, token_hash, agent_id, environment, name_prefix,
                                           max_uses, used_count, expires_at, created_at)
            VALUES (@Id, @TokenHash, @AgentId, @Environment, @NamePrefix,
                    @MaxUses, @UsedCount, @ExpiresAt, @CreatedAt)
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            token.Id,
            token.TokenHash,
            token.AgentId,
            token.Environment,
            token.NamePrefix,
            token.MaxUses,
            token.UsedCount,
            ExpiresAt = SqliteTimestamps.ToParameter(token.ExpiresAt),
            CreatedAt = SqliteTimestamps.ToParameter(token.CreatedAt),
        });
    }

    /// <inheritdoc />
    public async Task<EnrollmentToken?> FindByHashAsync(string tokenHash, CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT id, token_hash, agent_id, environment, name_prefix, max_uses, used_count,
                   expires_at, created_at
            FROM enrollment_tokens
            WHERE token_hash = @tokenHash
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<SqliteEnrollmentTokenRow>(
            sql,
            new { tokenHash });
        return row is null ? null : ToEnrollmentToken(row);
    }

    /// <inheritdoc />
    public async Task<bool> ConsumeAsync(Guid id, CancellationToken cancellationToken)
    {
        const string sql = """
            UPDATE enrollment_tokens
            SET used_count = used_count + 1
            WHERE id = @id AND used_count < max_uses
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var affectedRows = await connection.ExecuteAsync(sql, new { id });
        return affectedRows == 1;
    }

    private static EnrollmentToken ToEnrollmentToken(SqliteEnrollmentTokenRow row) => new()
    {
        Id = row.Id,
        TokenHash = row.TokenHash,
        AgentId = row.AgentId,
        Environment = row.Environment,
        NamePrefix = row.NamePrefix,
        MaxUses = row.MaxUses,
        UsedCount = row.UsedCount,
        ExpiresAt = SqliteTimestamps.FromColumn(row.ExpiresAt),
        CreatedAt = SqliteTimestamps.FromColumn(row.CreatedAt),
    };
}

/// <summary>Row shape of <c>enrollment_tokens</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteEnrollmentTokenRow
{
    public Guid Id { get; set; }

    public string TokenHash { get; set; } = string.Empty;

    public Guid? AgentId { get; set; }

    public string? Environment { get; set; }

    public string? NamePrefix { get; set; }

    public int MaxUses { get; set; }

    public int UsedCount { get; set; }

    public long ExpiresAt { get; set; }

    public long CreatedAt { get; set; }
}
