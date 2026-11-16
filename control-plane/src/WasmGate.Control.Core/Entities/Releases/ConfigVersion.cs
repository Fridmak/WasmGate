namespace WasmGate.Control.Core.Entities.Releases;

/// <summary>
/// An immutable configuration document. Versions are never edited in place:
/// a change produces a new version with a new SHA-256.
/// </summary>
/// <remarks>
/// The hash is unique per environment, so publishing the same YAML twice returns
/// the existing version instead of creating a duplicate.
/// </remarks>
public sealed class ConfigVersion
{
    /// <summary>Unique identifier of this configuration version.</summary>
    public Guid Id { get; init; } = Guid.NewGuid();

    /// <summary>Logical environment: prod, stage, lab.</summary>
    public required string Environment { get; init; }

    /// <summary>Lower-case hex SHA-256 of <see cref="Yaml"/>.</summary>
    public required string Sha256 { get; init; }

    /// <summary>Raw YAML. Validation is delegated to the Go data-plane validator.</summary>
    public required string Yaml { get; init; }

    /// <summary>Optional human-readable change description.</summary>
    public string? Message { get; init; }

    /// <summary>Identity of the principal that created the version.</summary>
    public required string CreatedBy { get; init; }

    /// <summary>Time when the version was created.</summary>
    public DateTimeOffset CreatedAt { get; init; } = DateTimeOffset.UtcNow;
}
