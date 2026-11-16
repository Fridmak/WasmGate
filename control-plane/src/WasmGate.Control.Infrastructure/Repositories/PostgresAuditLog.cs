using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Infrastructure.Repositories.Convert;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the append-only audit log.
/// </summary>
/// <remarks>
/// The log exposes no update or delete, so an event cannot be rewritten after it
/// is stored. <c>id</c> is a <c>BIGSERIAL</c> assigned by the database and also
/// defines the read order.
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
/// <remarks>
/// <c>details</c> is read as JSON text; <c>at</c> arrives as a UTC <see cref="DateTime"/>.
/// </remarks>
internal sealed class AuditEventRow
{
    public long Id { get; set; }

    public DateTime At { get; set; }

    public string Actor { get; set; } = string.Empty;

    public string Action { get; set; } = string.Empty;

    public string Target { get; set; } = string.Empty;

    public string? Details { get; set; }

    public string? RequestId { get; set; }
}
