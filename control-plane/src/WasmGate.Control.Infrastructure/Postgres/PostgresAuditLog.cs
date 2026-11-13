using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>PostgreSQL implementation of the append-only audit log.</summary>
public sealed class PostgresAuditLog(NpgsqlDataSource dataSource) : IAuditLog
{
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public Task AppendAsync(AuditEvent auditEvent, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: INSERT INTO audit_events (at, actor, action, target, details, request_id) VALUES (...)");

    /// <inheritdoc />
    public Task<IReadOnlyList<AuditEvent>> ListAsync(int limit, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM audit_events ORDER BY id DESC LIMIT @limit");
}
