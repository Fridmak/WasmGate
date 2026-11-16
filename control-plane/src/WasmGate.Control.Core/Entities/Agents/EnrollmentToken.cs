namespace WasmGate.Control.Core.Entities.Agents;

/// <summary>
/// A token that authorises enrollment. It is either bound to one agent
/// (<see cref="AgentId"/> set, single use) or shared by a rollout
/// (<see cref="AgentId"/> null, possibly scoped by a name prefix).
/// </summary>
/// <remarks>Only the SHA-256 hash of the raw token is stored.</remarks>
public sealed class EnrollmentToken
{
    /// <summary>Unique identifier of the token record.</summary>
    public Guid Id { get; init; } = Guid.NewGuid();

    /// <summary>SHA-256 hex of the raw token. The raw value is never stored.</summary>
    public required string TokenHash { get; init; }

    /// <summary>Agent the token is bound to, or null for a registration token.</summary>
    public Guid? AgentId { get; init; }

    /// <summary>Environment a newly enrolled agent is placed in.</summary>
    /// <remarks>
    /// Null on legacy tokens means <c>prod</c>. A token bound to an existing agent
    /// inherits that agent's environment.
    /// </remarks>
    public string? Environment { get; init; }

    /// <summary>Optional agent-name prefix a registration token is scoped to.</summary>
    /// <remarks>
    /// The prefix is compared with the final agent name, not with the agent
    /// identifier, and the comparison is ordinal. Null means the token is not
    /// scoped.
    /// </remarks>
    public string? NamePrefix { get; init; }

    /// <summary>How many enrollments the token allows.</summary>
    public int MaxUses { get; init; } = 1;

    /// <summary>Number of reserved uses.</summary>
    /// <remarks>
    /// A use is reserved before the certificate is issued, so a rejected CSR
    /// also increments this counter.
    /// </remarks>
    public int UsedCount { get; set; }

    /// <summary>Time after which the token is rejected.</summary>
    public DateTimeOffset ExpiresAt { get; init; }

    /// <summary>Time when the token was created.</summary>
    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;
}
