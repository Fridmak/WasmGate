using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory append-only audit log for the MVP.</summary>
public sealed class InMemoryAuditLog : IAuditLog
{
    private readonly ConcurrentQueue<AuditEvent> _events = new();
    private long _nextId;

    /// <inheritdoc />
    public Task AppendAsync(AuditEvent auditEvent, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _events.Enqueue(new AuditEvent
        {
            Id = Interlocked.Increment(ref _nextId),
            At = auditEvent.At,
            Actor = auditEvent.Actor,
            Action = auditEvent.Action,
            Target = auditEvent.Target,
            DetailsJson = auditEvent.DetailsJson,
            RequestId = auditEvent.RequestId,
        });
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task<IReadOnlyList<AuditEvent>> ListAsync(int limit, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult<IReadOnlyList<AuditEvent>>(
            _events.Reverse().Take(Math.Max(0, limit)).ToList());
    }
}
