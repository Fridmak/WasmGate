namespace WasmGate.Control.Core.Entities.Delivery;

/// <summary>
/// A request to put one release on a set of agents, split into waves.
/// </summary>
/// <remarks>
/// Waves are dispatched in order, but the control plane does not wait for one
/// wave to finish before starting the next. The deployment becomes terminal when
/// the first agent reports an apply result.
/// </remarks>
public sealed class Deployment
{
    /// <summary>Unique identifier of the deployment.</summary>
    public Guid Id { get; init; } = Guid.NewGuid();

    /// <summary>Logical environment this deployment targets: prod, stage, lab.</summary>
    public required string Environment { get; init; }

    /// <summary>Release being deployed.</summary>
    public required Guid ReleaseId { get; init; }

    /// <summary>Identity of the principal that started the deployment.</summary>
    public required string CreatedBy { get; init; }

    /// <summary>Current deployment status.</summary>
    public DeploymentStatus Status { get; set; } = DeploymentStatus.Pending;

    /// <summary>Waves in dispatch order.</summary>
    public IReadOnlyList<DeploymentWave> Waves { get; init; } = [];

    /// <summary>Time when the deployment was created.</summary>
    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Time when the deployment reached a terminal state.</summary>
    public DateTimeOffset? CompletedAt { get; set; }

    /// <summary>Human-readable outcome, usually the failure reason.</summary>
    /// <remarks>
    /// Null while the deployment is running or when it completed without an
    /// error. Set by the dispatcher when no command could be sent, by the agent
    /// result handler when an apply failed, and by the reaper on timeout.
    /// </remarks>
    public string? Message { get; set; }
}

/// <summary>
/// One rollout step. <see cref="Selector"/> is either <c>*</c> (all agents) or
/// a raw JSON object whose string properties must match the agent labels.
/// </summary>
public sealed class DeploymentWave
{
    /// <summary>0-based wave index; lower waves are dispatched first.</summary>
    public required int Index { get; init; }

    /// <summary>Either <c>*</c> or a JSON object used as a label selector.</summary>
    public required string Selector { get; init; }

    /// <summary>Current wave status.</summary>
    public DeploymentWaveStatus Status { get; set; } = DeploymentWaveStatus.Pending;

    /// <summary>Time when the wave started applying.</summary>
    public DateTimeOffset? StartedAt { get; set; }

    /// <summary>Time when the wave reached a terminal state.</summary>
    public DateTimeOffset? CompletedAt { get; set; }
}

/// <summary>Lifecycle state of a deployment.</summary>
public enum DeploymentStatus
{
    /// <summary>Created but not dispatched; the service currently starts in Running.</summary>
    Pending = 0,

    /// <summary>Commands were dispatched; waiting for the first apply result.</summary>
    Running = 1,

    /// <summary>An agent reported that it applied the release.</summary>
    Succeeded = 2,

    /// <summary>An agent reported a failure, or no connected agent matched the selectors.</summary>
    Failed = 3,

    /// <summary>The deployment was rolled back to the previous release.</summary>
    RolledBack = 4,

    /// <summary>Reserved for operator cancellation; not produced by the service yet.</summary>
    Cancelled = 5,
}

/// <summary>Lifecycle state of one deployment wave.</summary>
public enum DeploymentWaveStatus
{
    /// <summary>The wave has not started yet.</summary>
    Pending = 0,

    /// <summary>Commands have been sent to the agents of this wave.</summary>
    Running = 1,

    /// <summary>Reported as succeeded together with the deployment result.</summary>
    Succeeded = 2,

    /// <summary>Reported as failed together with the deployment result.</summary>
    Failed = 3,

    /// <summary>No agent matched the selector, so nothing was sent.</summary>
    Skipped = 4,
}
