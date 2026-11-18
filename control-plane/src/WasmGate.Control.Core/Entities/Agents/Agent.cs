namespace WasmGate.Control.Core.Entities.Agents;

/// <summary>
/// A data plane registered with the control plane: identity, labels, lifecycle
/// state and the release it reports.
/// </summary>
public sealed class Agent
{
    /// <summary>Unique identifier of the agent; also the identity in its certificate.</summary>
    public Guid Id { get; init; } = Guid.NewGuid();

    /// <summary>Operator-visible name, unique within the control plane.</summary>
    public required string Name { get; init; }

    /// <summary>Environment this agent belongs to; deployments are scoped to it.</summary>
    /// <remarks>
    /// Set when the agent is provisioned and never changed by the agent. An agent
    /// can only receive releases and deployments of its own environment.
    /// </remarks>
    public string Environment { get; init; } = "prod";

    /// <summary>Data-plane build version reported in Hello.</summary>
    public required string Version { get; init; }

    /// <summary>Plugin ABI versions supported by the data plane, for example v4.</summary>
    public IReadOnlyList<string> AbiVersions { get; init; } = [];

    /// <summary>Free-form labels matched by deployment wave selectors.</summary>
    public IReadOnlyDictionary<string, string> Labels { get; init; } = new Dictionary<string, string>();

    /// <summary>Current lifecycle state of the agent.</summary>
    public AgentStatus Status { get; set; } = AgentStatus.Pending;

    /// <summary>Time when the agent was first registered or enrolled.</summary>
    public DateTimeOffset RegisteredAt { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Time of the most recent heartbeat or control message.</summary>
    public DateTimeOffset LastSeenAt { get; set; } = DateTimeOffset.UtcNow;

    /// <summary>Release currently applied by the data plane, or null before the first deployment.</summary>
    public string? ActiveReleaseId { get; set; }

    /// <summary>SHA-256 of the active configuration document, or null when unknown.</summary>
    public string? ActiveConfigSha256 { get; set; }
}

/// <summary>Lifecycle state of a data-plane agent.</summary>
public enum AgentStatus
{
    /// <summary>Registered or enrolled, but no control session has been opened yet.</summary>
    Pending = 0,

    /// <summary>A control session is open, or the agent reported activity.</summary>
    Online = 1,

    /// <summary>The agent's last control session has ended.</summary>
    /// <remarks>
    /// There is no heartbeat timeout yet, so an agent whose connection silently
    /// dies stays Online until the transport fails.
    /// </remarks>
    Offline = 2,

    /// <summary>Reserved for graceful draining; not produced by the control plane yet.</summary>
    Draining = 3,

    /// <summary>Reserved for administrative shutdown; not produced by the control plane yet.</summary>
    Disabled = 4,
}
