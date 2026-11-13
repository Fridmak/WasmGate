namespace WasmGate.Control.Core.Entities;

/// <summary>
/// Append-only audit record. Every mutation of desired state, every apply
/// attempt and every rollback must leave one of these.
/// </summary>
public sealed class AuditEvent
{
    /// <summary>Database-assigned monotonic identifier; zero for a new event.</summary>
    public long Id { get; init; }

    /// <summary>Time when the event was recorded.</summary>
    public DateTimeOffset At { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Human, service account or agent that performed the action.</summary>
    public required string Actor { get; init; }

    /// <summary>Verb such as config.create, release.publish, deployment.start.</summary>
    public required string Action { get; init; }

    /// <summary>Entity identifier the action applies to.</summary>
    public required string Target { get; init; }

    /// <summary>Optional raw JSON payload: diff, error, release manifest.</summary>
    public string? DetailsJson { get; init; }

    /// <summary>Optional request identifier used to correlate API calls.</summary>
    public string? RequestId { get; init; }
}
