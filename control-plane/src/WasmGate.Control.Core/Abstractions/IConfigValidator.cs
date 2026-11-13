using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>
/// Validates a configuration document and its referenced artifacts before a
/// release is published. The reference implementation must delegate this to the
/// Go data plane (<c>wasmgate -validate</c>) instead of duplicating the YAML
/// schema and the WASM compiler in C#.
/// </summary>
public interface IConfigValidator
{
    /// <summary>Validates a configuration version and its release artifacts.</summary>
    /// <param name="config">Configuration version to validate.</param>
    /// <param name="artifacts">Artifacts referenced by the future release, including install paths.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Errors and warnings produced by the validator.</returns>
    Task<ValidationResult> ValidateAsync(
        ConfigVersion config,
        IReadOnlyCollection<ReleaseArtifact> artifacts,
        CancellationToken cancellationToken);
}
