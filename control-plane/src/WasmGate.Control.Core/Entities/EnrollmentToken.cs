namespace WasmGate.Control.Core.Entities;

/// <summary>
/// A one-time token a data plane uses to obtain its first certificate. Tokens
/// can be bound to a single agent (AgentId set) or shared by a rollout
/// (Registration kind, AgentId null, possibly scoped by name prefix).
/// </summary>
public sealed class EnrollmentToken
{
    /// <summary>Unique identifier of the token record.</summary>
    public Guid Id { get; init; } = Guid.NewGuid();

    /// <summary>SHA-256 hex of the raw token. The raw value is never stored.</summary>
    public required string TokenHash { get; init; }

    /// <summary>Agent the token is bound to, or null for a registration token.</summary>
    public Guid? AgentId { get; init; }

    /// <summary>Optional agent-name prefix a registration token is scoped to.</summary>
    /// <remarks>
    /// The prefix is compared with the final agent name, not with the agent
    /// identifier. A null value means that the token is not scoped.
    /// </remarks>
    public string? NamePrefix { get; init; }

    /// <summary>How many times the token may be used before it expires.</summary>
    public int MaxUses { get; init; } = 1;

    /// <summary>Number of successful enrollments already performed.</summary>
    public int UsedCount { get; set; }

    /// <summary>Time after which the token is rejected.</summary>
    public DateTimeOffset ExpiresAt { get; init; }

    /// <summary>Time when the token was created.</summary>
    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;
}
