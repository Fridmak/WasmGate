using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Core.Abstractions.Releases;

/// <summary>
/// Signs and verifies release manifests with a control-plane-owned key.
/// </summary>
/// <remarks>
/// The signature protects the release record: the workflow verifies it before
/// dispatching a deployment and refuses a release whose manifest no longer
/// matches. Data planes verify the SHA-256 of the configuration and of every
/// artifact they download, not this signature.
/// </remarks>
public interface IReleaseSigner
{
    /// <summary>Signs the canonical manifest of a release.</summary>
    /// <param name="release">Release to sign.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Base64 signature over the canonical manifest.</returns>
    Task<string> SignAsync(Release release, CancellationToken cancellationToken);

    /// <summary>Verifies a detached release signature.</summary>
    /// <param name="release">Release whose manifest is verified.</param>
    /// <param name="signature">Base64 signature produced by <see cref="SignAsync"/>.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>True when the signature matches; false for a malformed signature.</returns>
    /// <remarks>
    /// Called before every deployment. A rotated or lost signing key invalidates
    /// the signatures of existing releases, so keep the key with the data volume.
    /// </remarks>
    Task<bool> VerifyAsync(Release release, string signature, CancellationToken cancellationToken);
}
