using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Core.Entities.Delivery;

/// <summary>
/// Command pushed by the control plane to one connected agent. The type is
/// transport-neutral; the API layer maps it to the protobuf control message.
/// </summary>
/// <param name="Kind">Command kind.</param>
/// <param name="ReleaseId">Release to apply or roll back, when applicable.</param>
/// <param name="Reason">
/// Human-readable reason shown in audit and agent logs. For
/// <see cref="AgentCommandKind.SetLogLevel"/> it carries the level name.
/// </param>
/// <param name="TimeoutMs">Optional command timeout in milliseconds; zero means no timeout.</param>
/// <param name="ConfigSha256">SHA-256 of the release configuration.</param>
/// <param name="ConfigYaml">Release configuration body.</param>
/// <param name="Artifacts">Artifacts the data plane must fetch for the release.</param>
/// <param name="DeploymentId">Deployment that triggered the command.</param>
public sealed record AgentCommand(
    AgentCommandKind Kind,
    string? ReleaseId = null,
    string? Reason = null,
    int TimeoutMs = 0,
    string? ConfigSha256 = null,
    string? ConfigYaml = null,
    IReadOnlyList<AgentArtifact>? Artifacts = null,
    string? DeploymentId = null);

/// <summary>One artifact reference sent to a data plane.</summary>
/// <param name="Id">Content-addressed artifact identifier.</param>
/// <param name="Name">Install path inside the release.</param>
/// <param name="Kind">Artifact kind.</param>
/// <param name="Size">Artifact size in bytes.</param>
/// <param name="Signature">Optional artifact signature.</param>
public sealed record AgentArtifact(
    string Id,
    string Name,
    ArtifactKind Kind,
    long Size,
    string? Signature);

/// <summary>Kinds of commands that can be sent to a data-plane agent.</summary>
/// <remarks>
/// The transport understands every kind, but only
/// <see cref="ApplyRelease"/> is produced by the current release workflow.
/// </remarks>
public enum AgentCommandKind
{
    /// <summary>Apply a new release.</summary>
    ApplyRelease = 0,

    /// <summary>Stop accepting new connections and drain existing ones.</summary>
    Drain = 1,

    /// <summary>Change the data-plane log level.</summary>
    SetLogLevel = 2,

    /// <summary>Liveness probe over the control stream.</summary>
    Ping = 3,

    /// <summary>Roll back to a previous release.</summary>
    Rollback = 4,
}
