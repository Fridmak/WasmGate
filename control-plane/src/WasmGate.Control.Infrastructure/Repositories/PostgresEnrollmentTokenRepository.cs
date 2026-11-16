using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Repositories.Convert;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the enrollment-token store.
/// </summary>
/// <remarks>
/// Tokens outlive a process restart and are shared between control-plane
/// instances, so usage limits are enforced by the database. Consumption is a
/// single conditional <c>UPDATE</c>; reading the row and writing it back would
/// let two concurrent enrollments exceed <see cref="EnrollmentToken.MaxUses"/>.
/// </remarks>
public sealed class PostgresEnrollmentTokenRepository(NpgsqlDataSource dataSource) : IEnrollmentTokenRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task AddAsync(EnrollmentToken token, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO enrollment_tokens (id, token_hash, agent_id, environment, name_prefix,
                                           max_uses, used_count, expires_at, created_at)
            VALUES (@Id, @TokenHash, @AgentId, @Environment, @NamePrefix,
                    @MaxUses, @UsedCount, @ExpiresAt, @CreatedAt)
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            token.Id,
            token.TokenHash,
            token.AgentId,
            token.Environment,
            token.NamePrefix,
            token.MaxUses,
            token.UsedCount,
            ExpiresAt = PostgresTimestamps.ToParameter(token.ExpiresAt),
            CreatedAt = PostgresTimestamps.ToParameter(token.CreatedAt),
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

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var row = await connection.QuerySingleOrDefaultAsync<EnrollmentTokenRow>(sql, new { tokenHash });
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

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var affectedRows = await connection.ExecuteAsync(sql, new { id });
        return affectedRows == 1;
    }

    /// <summary>Maps one row of <c>enrollment_tokens</c> to a domain token.</summary>
    /// <param name="row">Row read from the database.</param>
    /// <returns>The mapped token.</returns>
    private static EnrollmentToken ToEnrollmentToken(EnrollmentTokenRow row) => new()
    {
        Id = row.Id,
        TokenHash = row.TokenHash,
        AgentId = row.AgentId,
        Environment = row.Environment,
        NamePrefix = row.NamePrefix,
        MaxUses = row.MaxUses,
        UsedCount = row.UsedCount,
        ExpiresAt = PostgresTimestamps.FromColumn(row.ExpiresAt),
        CreatedAt = PostgresTimestamps.FromColumn(row.CreatedAt),
    };
}

/// <summary>Row shape of <c>enrollment_tokens</c> as read by Dapper.AOT.</summary>
/// <remarks>Timestamps arrive as UTC <see cref="DateTime"/>.</remarks>
internal sealed class EnrollmentTokenRow
{
    public Guid Id { get; set; }

    public string TokenHash { get; set; } = string.Empty;

    public Guid? AgentId { get; set; }

    public string? Environment { get; set; }

    public string? NamePrefix { get; set; }

    public int MaxUses { get; set; }

    public int UsedCount { get; set; }

    public DateTime ExpiresAt { get; set; }

    public DateTime CreatedAt { get; set; }
}
