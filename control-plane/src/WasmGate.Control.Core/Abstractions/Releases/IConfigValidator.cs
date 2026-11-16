using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Core.Abstractions.Releases;

/// <summary>
/// Validates a configuration document and its artifacts before a release is
/// published.
/// </summary>
/// <remarks>
/// The C# side does not own the YAML schema or the WASM ABI. The implementation
/// writes the configuration and the artifacts to a temporary directory and runs
/// the Go data plane (<c>wasmgate -config … -validate</c>), so validation always
/// matches the runtime that will apply the release.
/// </remarks>
public interface IConfigValidator
{
    /// <summary>Validates a configuration version together with its artifacts.</summary>
    /// <param name="config">Configuration version to validate.</param>
    /// <param name="artifacts">Artifacts of the future release, including install paths.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// Errors and warnings. Warnings do not block a release; errors do.
    /// </returns>
    Task<ValidationResult> ValidateAsync(
        ConfigVersion config,
        IReadOnlyCollection<ReleaseArtifact> artifacts,
        CancellationToken cancellationToken);
}
