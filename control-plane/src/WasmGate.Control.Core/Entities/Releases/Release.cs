namespace WasmGate.Control.Core.Entities.Releases;

/// <summary>
/// A release is an immutable bundle: one configuration version plus all
/// artifacts it references. A release can be deployed to many agents.
/// </summary>
public sealed class Release
{
    /// <summary>Unique identifier of the release.</summary>
    public Guid Id { get; init; } = Guid.NewGuid();

    /// <summary>Logical environment this release belongs to.</summary>
    public required string Environment { get; init; }

    /// <summary>Configuration version included in the release.</summary>
    public required Guid ConfigVersionId { get; init; }

    /// <summary>SHA-256 of the included configuration document.</summary>
    public required string ConfigSha256 { get; init; }

    /// <summary>Artifacts included in the release and their install paths.</summary>
    public IReadOnlyList<ReleaseArtifact> Artifacts { get; init; } = [];

    /// <summary>Signature over the canonical release manifest.</summary>
    /// <remarks>
    /// Produced on creation. Data planes verify the configuration and artifact
    /// hashes, not this signature.
    /// </remarks>
    public string? Signature { get; set; }

    /// <summary>Current release lifecycle state.</summary>
    public ReleaseStatus Status { get; set; } = ReleaseStatus.Draft;

    /// <summary>Identity of the principal that created the release.</summary>
    public required string CreatedBy { get; init; }

    /// <summary>Time when the release was created.</summary>
    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;
}

/// <summary>Maps an artifact to the path the data plane must install it under.</summary>
public sealed class ReleaseArtifact
{
    /// <summary>Content-addressed artifact identifier (SHA-256).</summary>
    public required string ArtifactId { get; init; }

    /// <summary>Path inside the release, for example plugins/ipguard.wasm.</summary>
    public required string Name { get; init; }
}

/// <summary>Lifecycle state of a release.</summary>
/// <remarks>
/// The service stores a new release as Validated and only checks Revoked before
/// deploying; Draft and Published are not produced by the workflow.
/// </remarks>
public enum ReleaseStatus
{
    /// <summary>Reserved: a release that has not been validated.</summary>
    Draft = 0,

    /// <summary>The configuration and artifacts passed validation.</summary>
    Validated = 1,

    /// <summary>Reserved: a validated release marked ready for deployment.</summary>
    Published = 2,

    /// <summary>The release must no longer be deployed.</summary>
    Revoked = 3,
}
