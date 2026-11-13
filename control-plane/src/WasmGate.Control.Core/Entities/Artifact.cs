namespace WasmGate.Control.Core.Entities;

/// <summary>
/// An immutable, content-addressed artifact: a WASM plugin, a certificate or
/// another file that can be part of a release.
/// </summary>
public sealed class Artifact
{
    /// <summary>Lower-case hex SHA-256 of the content. This is the identity.</summary>
    public required string Id { get; init; }

    /// <summary>Semantic kind of the artifact.</summary>
    public required ArtifactKind Kind { get; init; }

    /// <summary>Artifact size in bytes.</summary>
    public required long Size { get; init; }

    /// <summary>Optional MIME type, for example application/wasm.</summary>
    public string? MediaType { get; init; }

    /// <summary>Optional detached signature over <see cref="Id"/>.</summary>
    public string? Signature { get; init; }

    /// <summary>Identity of the principal that uploaded the artifact.</summary>
    public string? CreatedBy { get; init; }

    /// <summary>Time when the artifact was stored.</summary>
    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;
}

/// <summary>Semantic kind of a release artifact.</summary>
public enum ArtifactKind
{
    /// <summary>Kind is not known or was not reported.</summary>
    Unspecified = 0,

    /// <summary>A WebAssembly plugin.</summary>
    Wasm = 1,

    /// <summary>A TLS certificate or certificate chain.</summary>
    Certificate = 2,

    /// <summary>A private key or other secret material.</summary>
    Key = 3,

    /// <summary>Any other file referenced by a release.</summary>
    Other = 4,
}
