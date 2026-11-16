namespace WasmGate.Control.Infrastructure.Validation;

/// <summary>
/// Maps an artifact install path from a release to a file inside a validation
/// directory.
/// </summary>
/// <remarks>
/// Install paths come from an API request, so they are treated as untrusted: a
/// rooted path or a path that resolves outside the root is rejected instead of
/// being normalized into place. Paths are portable POSIX-style, so a backslash,
/// a drive letter or a NUL byte is rejected on every platform; the same rule
/// applies in the data plane, which must behave identically on Linux and
/// Windows.
/// </remarks>
internal static class ReleasePath
{
    /// <summary>Combines the validation root with a relative install path.</summary>
    /// <param name="root">Temporary release root.</param>
    /// <param name="name">Artifact install path from the release.</param>
    /// <returns>Absolute target path inside <paramref name="root"/>.</returns>
    /// <exception cref="ArgumentException">
    /// The path is empty, rooted, contains a backslash or a drive letter, or
    /// escapes the release directory.
    /// </exception>
    /// <exception cref="NotSupportedException">
    /// The path contains characters unsupported by the platform.
    /// </exception>
    public static string Combine(string root, string name)
    {
        if (string.IsNullOrWhiteSpace(name))
        {
            throw new ArgumentException("Artifact path is required.", nameof(name));
        }

        if (name.Contains('\0'))
        {
            throw new ArgumentException($"Artifact path '{name}' contains a NUL byte.", nameof(name));
        }

        if (name.Contains('\\') || name.Contains(':'))
        {
            throw new ArgumentException(
                $"Artifact path '{name}' must use '/' separators and no drive letters.",
                nameof(name));
        }

        if (name.Any(c => c < ' ' || c == '\u007f'))
        {
            throw new ArgumentException(
                $"Artifact path '{name}' contains a control character.",
                nameof(name));
        }

        if (Path.IsPathRooted(name))
        {
            throw new ArgumentException($"Artifact path '{name}' must be relative.", nameof(name));
        }

        var normalizedRoot = Path.GetFullPath(root);
        var target = Path.GetFullPath(Path.Combine(normalizedRoot, name));
        var comparison = OperatingSystem.IsWindows()
            ? StringComparison.OrdinalIgnoreCase
            : StringComparison.Ordinal;
        var prefix = normalizedRoot.EndsWith(Path.DirectorySeparatorChar)
            ? normalizedRoot
            : normalizedRoot + Path.DirectorySeparatorChar;

        if (string.Equals(target, normalizedRoot, comparison) ||
            !target.StartsWith(prefix, comparison))
        {
            throw new ArgumentException(
                $"Artifact path '{name}' escapes the release directory.",
                nameof(name));
        }

        return target;
    }
}
