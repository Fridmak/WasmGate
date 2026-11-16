using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Repositories.Convert;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the issued-certificate store.
/// </summary>
/// <remarks>
/// <para>
/// Issued certificates are kept so they can be listed and revoked per agent. The
/// table is keyed by <c>(agent_id, serial_number)</c>; a thumbprint index supports
/// lookups by fingerprint.
/// </para>
/// <para>
/// "Active" means neither revoked nor expired. Expiry is compared with a
/// parameter rather than <c>now()</c>, so the result is independent of the
/// database clock.
/// </para>
/// </remarks>
public sealed class PostgresAgentCertificateRepository(NpgsqlDataSource dataSource) : IAgentCertificateRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO agent_certificates (agent_id, serial_number, thumbprint, pem,
                                            not_before, not_after, issued_at, revoked_at)
            VALUES (@AgentId, @SerialNumber, @Thumbprint, @Pem,
                    @NotBefore, @NotAfter, @IssuedAt, @RevokedAt)
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            certificate.AgentId,
            certificate.SerialNumber,
            certificate.Thumbprint,
            certificate.Pem,
            NotBefore = PostgresTimestamps.ToParameter(certificate.NotBefore),
            NotAfter = PostgresTimestamps.ToParameter(certificate.NotAfter),
            IssuedAt = PostgresTimestamps.ToParameter(certificate.IssuedAt),
            RevokedAt = PostgresTimestamps.ToParameter(certificate.RevokedAt),
        });
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<AgentCertificate>> ListActiveByAgentAsync(
        Guid agentId,
        CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT agent_id, serial_number, thumbprint, pem, not_before, not_after,
                   issued_at, revoked_at
            FROM agent_certificates
            WHERE agent_id = @agentId AND revoked_at IS NULL AND not_after > @now
            ORDER BY issued_at DESC
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var rows = await connection.QueryAsync<AgentCertificateRow>(sql, new
        {
            agentId,
            now = PostgresTimestamps.ToParameter(DateTimeOffset.UtcNow),
        });

        return rows.Select(ToAgentCertificate).ToList();
    }

    /// <inheritdoc />
    public async Task RevokeAsync(Guid agentId, string serialNumber, CancellationToken cancellationToken)
    {
        const string sql = """
            UPDATE agent_certificates
            SET revoked_at = @revokedAt
            WHERE agent_id = @agentId AND serial_number = @serialNumber AND revoked_at IS NULL
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            agentId,
            serialNumber,
            revokedAt = PostgresTimestamps.ToParameter(DateTimeOffset.UtcNow),
        });
    }

    /// <inheritdoc />
    public async Task<bool> IsActiveAsync(
        Guid agentId,
        string thumbprint,
        CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT EXISTS (
                SELECT 1
                FROM agent_certificates
                WHERE agent_id = @agentId
                  AND thumbprint = @thumbprint
                  AND revoked_at IS NULL
                  AND not_after > @now
            )
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        return await connection.ExecuteScalarAsync<bool>(sql, new
        {
            agentId,
            thumbprint,
            now = PostgresTimestamps.ToParameter(DateTimeOffset.UtcNow),
        });
    }

    /// <summary>Maps one row of <c>agent_certificates</c> to a domain certificate.</summary>
    /// <param name="row">Row read from the database.</param>
    /// <returns>The mapped certificate.</returns>
    private static AgentCertificate ToAgentCertificate(AgentCertificateRow row) => new()
    {
        AgentId = row.AgentId,
        SerialNumber = row.SerialNumber,
        Thumbprint = row.Thumbprint,
        Pem = row.Pem,
        NotBefore = PostgresTimestamps.FromColumn(row.NotBefore),
        NotAfter = PostgresTimestamps.FromColumn(row.NotAfter),
        IssuedAt = PostgresTimestamps.FromColumn(row.IssuedAt),
        RevokedAt = PostgresTimestamps.FromColumn(row.RevokedAt),
    };
}

/// <summary>Row shape of <c>agent_certificates</c> as read by Dapper.AOT.</summary>
/// <remarks>Timestamps arrive as UTC <see cref="DateTime"/> and are converted by <c>PostgresTimestamps</c>.</remarks>
internal sealed class AgentCertificateRow
{
    public Guid AgentId { get; set; }

    public string SerialNumber { get; set; } = string.Empty;

    public string Thumbprint { get; set; } = string.Empty;

    public string Pem { get; set; } = string.Empty;

    public DateTime NotBefore { get; set; }

    public DateTime NotAfter { get; set; }

    public DateTime IssuedAt { get; set; }

    public DateTime? RevokedAt { get; set; }
}
