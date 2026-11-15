using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>
/// PostgreSQL implementation of the append-only audit log.
/// </summary>
/// <remarks>
/// There is no update or delete: the audit trail must stay append-only, and the
/// database role used by the control plane should not have those grants.
/// <c>id</c> is <c>BIGSERIAL</c> and is assigned by the database.
/// </remarks>
public sealed class PostgresAuditLog(NpgsqlDataSource dataSource) : IAuditLog
{
    /// <summary>Upper bound for <see cref="ListAsync"/> so one request cannot read the whole table.</summary>
    private const int MaxLimit = 1000;

    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task AppendAsync(AuditEvent auditEvent, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO audit_events (at, actor, action, target, details, request_id)
            VALUES (@At, @Actor, @Action, @Target, @Details::jsonb, @RequestId)
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            At = PostgresTimestamps.ToParameter(auditEvent.At),
            auditEvent.Actor,
            auditEvent.Action,
            auditEvent.Target,
            Details = auditEvent.DetailsJson,
            auditEvent.RequestId,
        });
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<AuditEvent>> ListAsync(int limit, CancellationToken cancellationToken)
    {
        var boundedLimit = Math.Clamp(limit, 0, MaxLimit);
        if (boundedLimit == 0)
        {
            return [];
        }

        const string sql = """
            SELECT id, at, actor, action, target, details::text AS details, request_id
            FROM audit_events
            ORDER BY id DESC
            LIMIT @limit
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var rows = await connection.QueryAsync<AuditEventRow>(sql, new { limit = boundedLimit });
        return rows.Select(ToAuditEvent).ToList();
    }

    /// <summary>Maps one row of <c>audit_events</c> to a domain event.</summary>
    /// <param name="row">Row read from the database.</param>
    /// <returns>The mapped audit event.</returns>
    private static AuditEvent ToAuditEvent(AuditEventRow row) => new()
    {
        Id = row.Id,
        At = PostgresTimestamps.FromColumn(row.At),
        Actor = row.Actor,
        Action = row.Action,
        Target = row.Target,
        DetailsJson = row.Details,
        RequestId = row.RequestId,
    };
}

/// <summary>Row shape of <c>audit_events</c> as read by Dapper.AOT.</summary>
internal sealed class AuditEventRow
{
    /// <summary>Database-assigned identifier.</summary>
    public long Id { get; set; }

    /// <summary>Time when the event was recorded, in UTC.</summary>
    public DateTime At { get; set; }

    /// <summary>Principal that performed the action.</summary>
    public string Actor { get; set; } = string.Empty;

    /// <summary>Action verb, for example <c>agent.create</c>.</summary>
    public string Action { get; set; } = string.Empty;

    /// <summary>Affected entity identifier.</summary>
    public string Target { get; set; } = string.Empty;

    /// <summary>Raw JSON payload, or null.</summary>
    public string? Details { get; set; }

    /// <summary>Correlation identifier used by the caller, or null.</summary>
    public string? RequestId { get; set; }
}
