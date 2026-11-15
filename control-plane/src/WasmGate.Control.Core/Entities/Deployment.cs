namespace WasmGate.Control.Core.Entities;

/// <summary>
/// A request to put one release on a set of agents, split into waves.
/// The control plane advances a wave only after every agent in it has acknowledged
/// the release and the health gate has passed.
/// </summary>
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

    /// <summary>Ordered rollout waves.</summary>
    public IReadOnlyList<DeploymentWave> Waves { get; init; } = [];

    /// <summary>Time when the deployment was created.</summary>
    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;

    /// <summary>Time when the deployment reached a terminal state.</summary>
    public DateTimeOffset? CompletedAt { get; set; }
}

/// <summary>
/// One rollout step. <see cref="Selector"/> is either <c>*</c> (all agents) or
/// a raw JSON object whose string properties must match the agent labels.
/// Interpreting it belongs to the deployment service.
/// </summary>
public sealed class DeploymentWave
{
    /// <summary>0-based wave index; lower waves are applied first.</summary>
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
    /// <summary>The deployment has been created but not started.</summary>
    Pending = 0,

    /// <summary>At least one wave is being applied.</summary>
    Running = 1,

    /// <summary>All waves completed successfully.</summary>
    Succeeded = 2,

    /// <summary>A wave failed and the deployment stopped.</summary>
    Failed = 3,

    /// <summary>The deployment was rolled back to the previous release.</summary>
    RolledBack = 4,

    /// <summary>The deployment was cancelled by an operator.</summary>
    Cancelled = 5,
}

/// <summary>Lifecycle state of one deployment wave.</summary>
public enum DeploymentWaveStatus
{
    /// <summary>The wave has not started yet.</summary>
    Pending = 0,

    /// <summary>The wave is being applied to its agents.</summary>
    Running = 1,

    /// <summary>All agents in the wave acknowledged the release.</summary>
    Succeeded = 2,

    /// <summary>At least one agent failed to apply the release.</summary>
    Failed = 3,

    /// <summary>The wave was skipped because a previous wave failed.</summary>
    Skipped = 4,
}
