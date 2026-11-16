using System.Text;
using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Infrastructure.Security;

/// <summary>
/// Deterministic byte representation of a release that the manifest signature
/// covers.
/// </summary>
/// <remarks>
/// The format is a line-oriented text document starting with a version marker,
/// followed by the release id, environment, configuration version, configuration
/// hash, and one name/hash pair per artifact sorted by install path. Changing the
/// order or the marker invalidates every existing signature, so treat it as a
/// wire format.
/// </remarks>
internal static class ReleaseManifest
{
    /// <summary>Builds the canonical representation covered by the signature.</summary>
    /// <param name="release">Release to serialize.</param>
    /// <returns>UTF-8 bytes of the canonical manifest.</returns>
    public static byte[] Canonicalize(Release release)
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
