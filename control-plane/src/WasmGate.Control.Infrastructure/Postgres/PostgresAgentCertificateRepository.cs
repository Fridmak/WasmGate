using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>
/// PostgreSQL implementation of the issued-certificate store.
/// </summary>
/// <remarks>
/// <para>
/// Keeping issued certificates lets the control plane revoke an agent and later
/// build a CRL. The table is keyed by <c>(agent_id, serial_number)</c>; the
/// thumbprint index supports lookups by certificate fingerprint.
/// </para>
/// <para>
/// "Active" means not revoked and not expired. The current time is passed as a
/// parameter instead of using <c>now()</c>, so the adapter behaves like the
/// in-memory one and stays testable.
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
internal sealed class AgentCertificateRow
{
    /// <summary>Agent the certificate belongs to.</summary>
    public Guid AgentId { get; set; }

    /// <summary>Hex serial number.</summary>
    public string SerialNumber { get; set; } = string.Empty;

    /// <summary>SHA-256 fingerprint.</summary>
    public string Thumbprint { get; set; } = string.Empty;

    /// <summary>PEM-encoded certificate.</summary>
    public string Pem { get; set; } = string.Empty;

    /// <summary>Start of the validity period, in UTC.</summary>
    public DateTime NotBefore { get; set; }

    /// <summary>End of the validity period, in UTC.</summary>
    public DateTime NotAfter { get; set; }

    /// <summary>Issue time, in UTC.</summary>
    public DateTime IssuedAt { get; set; }

    /// <summary>Revocation time, in UTC, or null.</summary>
    public DateTime? RevokedAt { get; set; }
}
