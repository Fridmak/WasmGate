using System.Security.Cryptography;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Infrastructure.Security;

/// <summary>
/// ECDSA P-256 signer for release manifests.
/// </summary>
/// <remarks>
/// <para>
/// The key is an unencrypted PKCS#8 PEM file; on Unix it is written with
/// owner-only permissions. The matching public key is written next to it as
/// <c>&lt;path&gt;.pub</c> in SubjectPublicKeyInfo PEM format for external
/// verification.
/// </para>
/// <para>
/// Signatures are produced over <see cref="ReleaseManifest"/> in the IEEE P1363
/// format, so signing and verification cannot disagree about the covered bytes.
/// </para>
/// </remarks>
public sealed class EcdsaReleaseSigner : IReleaseSigner
{
    private readonly ECDsa _key;

    /// <summary>Loads the signing key or generates one on first use.</summary>
    /// <param name="options">Control-plane options containing the key path.</param>
    public EcdsaReleaseSigner(ControlPlaneOptions options)
    {
        var path = string.IsNullOrWhiteSpace(options.SigningKeyPath)
            ? "data/keys/release-signing.key"
            : options.SigningKeyPath;

        if (File.Exists(path))
        {
            _key = ECDsa.Create();
            _key.ImportFromPem(File.ReadAllText(path));
            return;
        }

        var directory = Path.GetDirectoryName(Path.GetFullPath(path));
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        _key = ECDsa.Create(ECCurve.NamedCurves.nistP256);
        File.WriteAllText(path, _key.ExportPkcs8PrivateKeyPem());
        if (!OperatingSystem.IsWindows())
        {
            File.SetUnixFileMode(path, UnixFileMode.UserRead | UnixFileMode.UserWrite);
        }

        File.WriteAllText(path + ".pub", _key.ExportSubjectPublicKeyInfoPem());
    }

    /// <inheritdoc />
    public Task<string> SignAsync(Release release, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        var signature = _key.SignData(
            ReleaseManifest.Canonicalize(release),
            HashAlgorithmName.SHA256,
            DSASignatureFormat.IeeeP1363FixedFieldConcatenation);
        return Task.FromResult(Convert.ToBase64String(signature));
    }

    /// <inheritdoc />
    public Task<bool> VerifyAsync(Release release, string signature, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        try
        {
            return Task.FromResult(_key.VerifyData(
                ReleaseManifest.Canonicalize(release),
                Convert.FromBase64String(signature),
                HashAlgorithmName.SHA256,
                DSASignatureFormat.IeeeP1363FixedFieldConcatenation));
        }
        catch (Exception ex) when (ex is FormatException or CryptographicException)
        {
            return Task.FromResult(false);
        }
    }
}
