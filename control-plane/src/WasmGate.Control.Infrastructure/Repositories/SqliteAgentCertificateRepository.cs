using Dapper;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the issued-certificate store.
/// </summary>
/// <remarks>
/// "Active" means neither revoked nor expired. Expiry is compared with an
/// application-provided Unix-millisecond value, so the result does not depend on
/// the database clock.
/// </remarks>
public sealed class SqliteAgentCertificateRepository(SqliteConnectionFactory connections) : IAgentCertificateRepository
{
    private SqliteConnectionFactory Connections { get; } = connections;

    /// <inheritdoc />
    public async Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO agent_certificates (agent_id, serial_number, thumbprint, pem,
                                            not_before, not_after, issued_at, revoked_at)
            VALUES (@AgentId, @SerialNumber, @Thumbprint, @Pem,
                    @NotBefore, @NotAfter, @IssuedAt, @RevokedAt)
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            certificate.AgentId,
            certificate.SerialNumber,
            certificate.Thumbprint,
            certificate.Pem,
            NotBefore = SqliteTimestamps.ToParameter(certificate.NotBefore),
            NotAfter = SqliteTimestamps.ToParameter(certificate.NotAfter),
            IssuedAt = SqliteTimestamps.ToParameter(certificate.IssuedAt),
            RevokedAt = SqliteTimestamps.ToParameter(certificate.RevokedAt),
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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var rows = await connection.QueryAsync<SqliteAgentCertificateRow>(sql, new
        {
            agentId,
            now = SqliteTimestamps.ToParameter(DateTimeOffset.UtcNow),
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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            agentId,
            serialNumber,
            revokedAt = SqliteTimestamps.ToParameter(DateTimeOffset.UtcNow),
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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        return await connection.ExecuteScalarAsync<bool>(sql, new
        {
            agentId,
            thumbprint,
            now = SqliteTimestamps.ToParameter(DateTimeOffset.UtcNow),
        });
    }

    private static AgentCertificate ToAgentCertificate(SqliteAgentCertificateRow row) => new()
    {
        AgentId = row.AgentId,
        SerialNumber = row.SerialNumber,
        Thumbprint = row.Thumbprint,
        Pem = row.Pem,
        NotBefore = SqliteTimestamps.FromColumn(row.NotBefore),
        NotAfter = SqliteTimestamps.FromColumn(row.NotAfter),
        IssuedAt = SqliteTimestamps.FromColumn(row.IssuedAt),
        RevokedAt = SqliteTimestamps.FromColumn(row.RevokedAt),
    };
}

/// <summary>Row shape of <c>agent_certificates</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteAgentCertificateRow
{
    public Guid AgentId { get; set; }

    public string SerialNumber { get; set; } = string.Empty;

    public string Thumbprint { get; set; } = string.Empty;

    public string Pem { get; set; } = string.Empty;

    public long NotBefore { get; set; }

    public long NotAfter { get; set; }

    public long IssuedAt { get; set; }

    public long? RevokedAt { get; set; }
}
