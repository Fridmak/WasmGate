using System.Text.Json.Serialization;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Api.Endpoints;

/// <summary>Request to create one agent.</summary>
/// <param name="Name">Unique agent name.</param>
/// <param name="Labels">Labels matched by deployment wave selectors.</param>
public sealed record CreateAgentRequest(string Name, Dictionary<string, string>? Labels);

/// <summary>Result of creating one agent.</summary>
/// <param name="AgentId">Identifier of the created agent.</param>
/// <param name="EnrollToken">One-time enrollment token, shown only once.</param>
public sealed record CreateAgentResponse(Guid AgentId, string EnrollToken);

/// <summary>Request to create a shared registration token.</summary>
/// <param name="NamePrefix">
/// Optional prefix that every enrolling agent name must start with; null or
/// empty means the token is not scoped.
/// </param>
/// <param name="MaxUses">Maximum number of enrollments.</param>
/// <param name="TtlSeconds">How long the token stays valid, in seconds.</param>
public sealed record CreateRegistrationTokenRequest(string? NamePrefix, int MaxUses, int TtlSeconds);

/// <summary>Result of creating a registration token.</summary>
/// <param name="Token">Raw registration token.</param>
/// <param name="MaxUses">Maximum number of enrollments.</param>
/// <param name="ExpiresAt">When the token stops working.</param>
public sealed record CreateRegistrationTokenResponse(string Token, int MaxUses, DateTimeOffset ExpiresAt);

/// <summary>Request to upload one artifact.</summary>
/// <param name="Kind">Artifact kind.</param>
/// <param name="MediaType">Optional MIME type.</param>
/// <param name="ContentBase64">Base64-encoded artifact bytes.</param>
public sealed record UploadArtifactRequest(ArtifactKind Kind, string? MediaType, string ContentBase64);

/// <summary>Result of uploading an artifact.</summary>
/// <param name="Id">Content-addressed identifier.</param>
/// <param name="Size">Stored size in bytes.</param>
public sealed record UploadArtifactResponse(string Id, long Size);

/// <summary>Request to create a configuration version.</summary>
/// <param name="Environment">Logical environment.</param>
/// <param name="Yaml">Raw YAML configuration.</param>
/// <param name="Message">Optional change description.</param>
public sealed record CreateConfigRequest(string Environment, string Yaml, string? Message);

/// <summary>Result of creating a configuration version.</summary>
/// <param name="Id">Configuration version identifier.</param>
/// <param name="Sha256">Content hash.</param>
public sealed record CreateConfigResponse(Guid Id, string Sha256);

/// <summary>One artifact inside a release request.</summary>
/// <param name="ArtifactId">Uploaded artifact identifier.</param>
/// <param name="Name">Install path inside the release.</param>
public sealed record CreateReleaseArtifact(string ArtifactId, string Name);

/// <summary>Request to create a release.</summary>
/// <param name="Environment">Logical environment.</param>
/// <param name="ConfigVersionId">Configuration version to include.</param>
/// <param name="Artifacts">Artifacts to include; null is treated as an empty list.</param>
public sealed record CreateReleaseRequest(
    string Environment,
    Guid ConfigVersionId,
    List<CreateReleaseArtifact>? Artifacts);

/// <summary>Result of creating a release.</summary>
/// <param name="Id">Release identifier.</param>
/// <param name="Signature">Manifest signature.</param>
public sealed record CreateReleaseResponse(Guid Id, string? Signature);

/// <summary>Request to start a deployment.</summary>
/// <param name="Environment">Logical environment.</param>
/// <param name="ReleaseId">Release to deploy.</param>
/// <param name="WaveSelectors">JSON label selectors, one per wave; empty means all agents.</param>
public sealed record CreateDeploymentRequest(
    string Environment,
    Guid ReleaseId,
    List<string>? WaveSelectors);

/// <summary>Result of starting a deployment.</summary>
/// <param name="Id">Deployment identifier.</param>
/// <param name="Status">Initial deployment status.</param>
public sealed record CreateDeploymentResponse(Guid Id, DeploymentStatus Status);

/// <summary>Compact agent row for the admin API.</summary>
/// <param name="Id">Agent identifier.</param>
/// <param name="Name">Agent name.</param>
/// <param name="Status">Agent status.</param>
/// <param name="LastSeenAt">Time of the last heartbeat or control message.</param>
/// <param name="ActiveReleaseId">Release the agent reports as applied, if any.</param>
/// <param name="Labels">Labels matched by wave selectors.</param>
public sealed record AgentSummary(
    Guid Id,
    string Name,
    AgentStatus Status,
    DateTimeOffset LastSeenAt,
    string? ActiveReleaseId,
    IReadOnlyDictionary<string, string> Labels);

/// <summary>Result of issuing a replacement enrollment token.</summary>
/// <param name="Token">Raw single-use token, shown only here.</param>
/// <param name="ExpiresAt">When the token stops working.</param>
public sealed record CreateAgentTokenResponse(string Token, DateTimeOffset ExpiresAt);

/// <summary>Compact view of one active agent certificate.</summary>
/// <param name="SerialNumber">Certificate serial number, used for revocation.</param>
/// <param name="Thumbprint">SHA-256 fingerprint of the DER certificate.</param>
/// <param name="NotBefore">Start of the validity period.</param>
/// <param name="NotAfter">End of the validity period.</param>
/// <param name="IssuedAt">Time when the control plane issued it.</param>
public sealed record AgentCertificateSummary(
    string SerialNumber,
    string Thumbprint,
    DateTimeOffset NotBefore,
    DateTimeOffset NotAfter,
    DateTimeOffset IssuedAt);

[JsonSerializable(typeof(CreateAgentRequest))]
[JsonSerializable(typeof(CreateAgentResponse))]
[JsonSerializable(typeof(CreateAgentTokenResponse))]
[JsonSerializable(typeof(List<AgentCertificateSummary>))]
[JsonSerializable(typeof(CreateRegistrationTokenRequest))]
[JsonSerializable(typeof(CreateRegistrationTokenResponse))]
[JsonSerializable(typeof(UploadArtifactRequest))]
[JsonSerializable(typeof(UploadArtifactResponse))]
[JsonSerializable(typeof(CreateConfigRequest))]
[JsonSerializable(typeof(CreateConfigResponse))]
[JsonSerializable(typeof(CreateReleaseRequest))]
[JsonSerializable(typeof(CreateReleaseResponse))]
[JsonSerializable(typeof(CreateDeploymentRequest))]
[JsonSerializable(typeof(CreateDeploymentResponse))]
[JsonSerializable(typeof(AgentSummary))]
[JsonSerializable(typeof(List<AgentSummary>))]
[JsonSerializable(typeof(List<ConfigVersion>))]
[JsonSerializable(typeof(List<Release>))]
[JsonSerializable(typeof(List<Deployment>))]
[JsonSerializable(typeof(List<Artifact>))]
[JsonSerializable(typeof(IReadOnlyList<Artifact>))]
[JsonSerializable(typeof(List<AuditEvent>))]
[JsonSerializable(typeof(IReadOnlyList<ConfigVersion>))]
[JsonSerializable(typeof(IReadOnlyList<Release>))]
[JsonSerializable(typeof(IReadOnlyList<Deployment>))]
[JsonSerializable(typeof(IReadOnlyList<AuditEvent>))]
internal sealed partial class AdminJsonContext : JsonSerializerContext;
