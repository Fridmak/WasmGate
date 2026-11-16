namespace WasmGate.Control.Core.Entities;

/// <summary>
/// Append-only audit record. Every change of desired state and every applied
/// result leaves one of these.
/// </summary>
public sealed class AuditEvent
{
    /// <summary>Database-assigned identifier; zero for a new event.</summary>
    public long Id { get; init; }

    /// <summary>Time when the event was recorded.</summary>
    public DateTimeOffset At { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Human, service account or agent that performed the action.</summary>
    public required string Actor { get; init; }

    /// <summary>
    /// Stable verb, for example <c>agent.create</c>, <c>agent.enroll</c>,
    /// <c>config.create</c>, <c>release.create</c>, <c>deployment.start</c> or
    /// <c>deployment.apply</c>.
    /// </summary>
    public required string Action { get; init; }

    /// <summary>Entity identifier the action applies to.</summary>
    public required string Target { get; init; }

    /// <summary>Optional raw JSON payload such as an error or a manifest.</summary>
    /// <remarks>The current workflow does not fill this field yet.</remarks>
    public string? DetailsJson { get; init; }

    /// <summary>Optional request identifier used to correlate API calls.</summary>
    /// <remarks>The current workflow does not fill this field yet.</remarks>
    public string? RequestId { get; init; }
}
