using System.Security.Cryptography;
using System.Text;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Security;

/// <summary>
/// ECDSA P-256 signer for release manifests. The private key stays on the
/// control plane; the derived public key can be shipped to data planes for
/// verification.
/// </summary>
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
            CanonicalManifest(release),
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
                CanonicalManifest(release),
                Convert.FromBase64String(signature),
                HashAlgorithmName.SHA256,
                DSASignatureFormat.IeeeP1363FixedFieldConcatenation));
        }
        catch (FormatException)
        {
            return Task.FromResult(false);
        }
    }

    /// <summary>Builds the deterministic byte representation covered by the signature.</summary>
    /// <param name="release">Release to serialize.</param>
    private static byte[] CanonicalManifest(Release release)
    {
        var builder = new StringBuilder();
        builder.Append("wasmgate-release-v1\n");
        builder.Append(release.Id).Append('\n');
        builder.Append(release.Environment).Append('\n');
        builder.Append(release.ConfigVersionId).Append('\n');
        builder.Append(release.ConfigSha256).Append('\n');

        foreach (var artifact in release.Artifacts.OrderBy(a => a.Name, StringComparer.Ordinal))
        {
            builder.Append(artifact.Name).Append('\n');
            builder.Append(artifact.ArtifactId).Append('\n');
        }

        return Encoding.UTF8.GetBytes(builder.ToString());
    }
}
