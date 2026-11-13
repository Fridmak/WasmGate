namespace WasmGate.Control.Core.Entities;

/// <summary>
/// A data-plane process registered with the control plane. The control plane
/// owns the fleet inventory; the data plane owns the actual traffic.
/// </summary>
public sealed class Agent
{
    /// <summary>Unique identifier of the agent.</summary>
    public Guid Id { get; init; } = Guid.NewGuid();

    /// <summary>Operator-visible name, unique within the control plane.</summary>
    public required string Name { get; init; }

    /// <summary>Data-plane build version reported in Hello.</summary>
    public required string Version { get; init; }

    /// <summary>ABI versions supported by the data plane, for example v1, v2, v3.</summary>
    public IReadOnlyList<string> AbiVersions { get; init; } = [];

    /// <summary>Free-form labels used by deployment selectors (region, role, canary).</summary>
    public IReadOnlyDictionary<string, string> Labels { get; init; } = new Dictionary<string, string>();

    /// <summary>Current lifecycle state of the agent.</summary>
    public AgentStatus Status { get; set; } = AgentStatus.Pending;

    /// <summary>Time when the agent first connected to the control plane.</summary>
    public DateTimeOffset RegisteredAt { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Time of the most recent heartbeat or control message.</summary>
    public DateTimeOffset LastSeenAt { get; set; } = DateTimeOffset.UtcNow;

    /// <summary>Release currently applied by the data plane, or null during bootstrap.</summary>
    public string? ActiveReleaseId { get; set; }

    /// <summary>SHA-256 of the active configuration document.</summary>
    public string? ActiveConfigSha256 { get; set; }
}

/// <summary>Lifecycle state of a data-plane agent.</summary>
public enum AgentStatus
{
    /// <summary>The agent has been registered but has not completed its first handshake.</summary>
    Pending = 0,

    /// <summary>The agent has an active control session.</summary>
    Online = 1,

    /// <summary>The agent has not been seen for longer than the configured timeout.</summary>
    Offline = 2,

    /// <summary>The agent is draining connections and should not receive new work.</summary>
    Draining = 3,

    /// <summary>The agent is administratively disabled.</summary>
    Disabled = 4,
}
