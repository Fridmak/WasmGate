using System.Text;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Security;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Tests for the signed release manifest and its signer.</summary>
public sealed class ReleaseManifestTests
{
    private static Release NewRelease(params (string Name, string Id)[] artifacts) =>
        NewRelease(new string('a', 64), artifacts);

    private static Release NewRelease(string configSha256, params (string Name, string Id)[] artifacts) => new()
    {
        Environment = "prod",
        ConfigVersionId = Guid.Parse("7c2e5a10-1b3c-4d5e-8f90-2a3b4c5d6e7f"),
        ConfigSha256 = configSha256,
        CreatedBy = "tests",
        Artifacts = artifacts
            .Select(a => new ReleaseArtifact { ArtifactId = a.Id, Name = a.Name })
            .ToList(),
    };

    private static EcdsaReleaseSigner NewSigner(out string directory)
    {
        directory = Directory.CreateTempSubdirectory().FullName;
        return new EcdsaReleaseSigner(new ControlPlaneOptions
        {
            SigningKeyPath = Path.Combine(directory, "release-signing.key"),
        });
    }

    [Fact]
    public async Task SignAndVerifyRoundTrip()
    {
        var signer = NewSigner(out var directory);
        try
        {
            var release = NewRelease(("plugins/ipguard.wasm", new string('b', 64)));

            var signature = await signer.SignAsync(release, CancellationToken.None);

            Assert.False(string.IsNullOrEmpty(signature));
            Assert.True(await signer.VerifyAsync(release, signature, CancellationToken.None));
            Assert.True(File.Exists(Path.Combine(directory, "release-signing.key")));
            Assert.True(File.Exists(Path.Combine(directory, "release-signing.key.pub")));
        }
        finally
        {
            Directory.Delete(directory, recursive: true);
        }
    }

    [Fact]
    public async Task DetectsTamperedConfigurationHash()
    {
        var signer = NewSigner(out var directory);
        try
        {
            var release = NewRelease();
            var signature = await signer.SignAsync(release, CancellationToken.None);

            var tampered = NewRelease(new string('c', 64));

            Assert.False(await signer.VerifyAsync(tampered, signature, CancellationToken.None));
        }
        finally
        {
            Directory.Delete(directory, recursive: true);
        }
    }

    [Fact]
    public async Task DetectsTamperedArtifact()
    {
        var signer = NewSigner(out var directory);
        try
        {
            var release = NewRelease(("plugins/ipguard.wasm", new string('b', 64)));
            var signature = await signer.SignAsync(release, CancellationToken.None);

            var tampered = NewRelease(("plugins/ipguard.wasm", new string('d', 64)));

            Assert.False(await signer.VerifyAsync(tampered, signature, CancellationToken.None));
        }
        finally
        {
            Directory.Delete(directory, recursive: true);
        }
    }

    [Fact]
    public async Task RejectsMalformedSignature()
    {
        var signer = NewSigner(out var directory);
        try
        {
            var release = NewRelease();

            Assert.False(await signer.VerifyAsync(release, "not-base64", CancellationToken.None));
        }
        finally
        {
            Directory.Delete(directory, recursive: true);
        }
    }

    [Fact]
    public void CanonicalManifestIsDeterministicAndSorted()
    {
        var release = NewRelease(
            ("plugins/z.wasm", new string('b', 64)),
            ("plugins/a.wasm", new string('c', 64)));

        var text = Encoding.UTF8.GetString(ReleaseManifest.Canonicalize(release));

        Assert.StartsWith("wasmgate-release-v1\n", text, StringComparison.Ordinal);
        Assert.Contains(release.Id.ToString(), text, StringComparison.Ordinal);
        Assert.Contains(release.ConfigSha256, text, StringComparison.Ordinal);
        Assert.True(
            text.IndexOf("plugins/a.wasm", StringComparison.Ordinal) <
            text.IndexOf("plugins/z.wasm", StringComparison.Ordinal),
            "artifacts must be sorted by install path");
    }
}
