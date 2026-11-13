using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>Append-only audit trail for control-plane operations.</summary>
public interface IAuditLog
{
    /// <summary>Appends one audit event.</summary>
    /// <param name="auditEvent">Event to append.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AppendAsync(AuditEvent auditEvent, CancellationToken cancellationToken);

    /// <summary>Returns the most recent audit events.</summary>
    /// <param name="limit">Maximum number of events to return.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Audit events ordered newest first.</returns>
    Task<IReadOnlyList<AuditEvent>> ListAsync(int limit, CancellationToken cancellationToken);
}
