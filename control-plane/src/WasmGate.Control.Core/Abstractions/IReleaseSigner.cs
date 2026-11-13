using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>
/// Signs and verifies release manifests. The data plane verifies the signature
/// before applying a release, so a compromised control plane alone is not
/// enough to push arbitrary WASM code.
/// </summary>
public interface IReleaseSigner
{
    /// <summary>Signs the canonical manifest of a release.</summary>
    /// <param name="release">Release to sign.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Detached signature encoded for transport and storage.</returns>
    Task<string> SignAsync(Release release, CancellationToken cancellationToken);

    /// <summary>Verifies a detached release signature.</summary>
    /// <param name="release">Release whose manifest is verified.</param>
    /// <param name="signature">Detached signature to verify.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>True when the signature matches the release manifest.</returns>
    Task<bool> VerifyAsync(Release release, string signature, CancellationToken cancellationToken);
}
