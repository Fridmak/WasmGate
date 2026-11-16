using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>Append-only audit trail of control-plane operations.</summary>
/// <remarks>
/// Events are only added, never updated or deleted, so the trail can be used to
/// reconstruct who changed which release, agent or deployment.
/// </remarks>
public interface IAuditLog
{
    /// <summary>Appends one audit event.</summary>
    /// <param name="auditEvent">Event to append; actor, action and target are set by the caller.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AppendAsync(AuditEvent auditEvent, CancellationToken cancellationToken);

    /// <summary>Returns the most recent audit events.</summary>
    /// <param name="limit">Maximum number of events to return.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Audit events ordered newest first.</returns>
    Task<IReadOnlyList<AuditEvent>> ListAsync(int limit, CancellationToken cancellationToken);
}
