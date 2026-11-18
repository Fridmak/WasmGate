using Dapper;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the append-only audit log.
/// </summary>
/// <remarks>
/// The log exposes no update or delete. <c>id</c> is assigned by SQLite and also
/// defines the read order, matching the PostgreSQL <c>BIGSERIAL</c> behaviour.
/// </remarks>
public sealed class SqliteAuditLog(SqliteConnectionFactory connections) : IAuditLog
{
    private const int MaxLimit = 1000;

    private SqliteConnectionFactory Connections { get; } = connections;

    /// <inheritdoc />
    public async Task AppendAsync(AuditEvent auditEvent, CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO audit_events (at, actor, action, target, details, request_id)
            VALUES (@At, @Actor, @Action, @Target, @Details, @RequestId)
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await connection.ExecuteAsync(sql, new
        {
            At = SqliteTimestamps.ToParameter(auditEvent.At),
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
            SELECT id, at, actor, action, target, details, request_id
            FROM audit_events
            ORDER BY id DESC
            LIMIT @limit
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var rows = await connection.QueryAsync<SqliteAuditEventRow>(sql, new { limit = boundedLimit });
        return rows.Select(ToAuditEvent).ToList();
    }

    private static AuditEvent ToAuditEvent(SqliteAuditEventRow row) => new()
    {
        Id = row.Id,
        At = SqliteTimestamps.FromColumn(row.At),
        Actor = row.Actor,
        Action = row.Action,
        Target = row.Target,
        DetailsJson = row.Details,
        RequestId = row.RequestId,
    };
}

/// <summary>Row shape of <c>audit_events</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteAuditEventRow
{
    public long Id { get; set; }

    public long At { get; set; }

    public string Actor { get; set; } = string.Empty;

    public string Action { get; set; } = string.Empty;

    public string Target { get; set; } = string.Empty;

    public string? Details { get; set; }

    public string? RequestId { get; set; }
}
