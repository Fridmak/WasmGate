using System.Diagnostics;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Validation;

/// <summary>
/// Delegates validation to the Go data-plane binary. This is the single source
/// of truth for the YAML schema, WASM ABI and TLS material; do not reimplement
/// those rules in C#.
/// </summary>
public sealed class GoConfigValidator(
    ControlPlaneOptions options,
    IArtifactStore artifacts) : IConfigValidator
{
    /// <summary>File name reserved for the configuration document inside the temporary tree.</summary>
    private const string ConfigFileName = "wasmgate.yaml";

    /// <inheritdoc />
    public async Task<ValidationResult> ValidateAsync(
        ConfigVersion config,
        IReadOnlyCollection<ReleaseArtifact> releaseArtifacts,
        CancellationToken cancellationToken)
    {
        var result = new ValidationResult();
        var tempRoot = Path.Combine(Path.GetTempPath(), $"wasmgate-validate-{Guid.NewGuid():N}");
        Directory.CreateDirectory(tempRoot);

        try
        {
            var configPath = Path.Combine(tempRoot, ConfigFileName);
            await File.WriteAllTextAsync(configPath, config.Yaml, cancellationToken);

            var names = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
            foreach (var artifact in releaseArtifacts)
            {
                if (string.IsNullOrWhiteSpace(artifact.Name) || !names.Add(artifact.Name))
                {
                    result.AddError(
                        "validator.artifact_name",
                        $"Artifact path '{artifact.Name}' is empty or duplicated.");
                    return result;
                }

                if (string.Equals(artifact.Name, ConfigFileName, StringComparison.OrdinalIgnoreCase))
                {
                    result.AddError(
                        "validator.artifact_name",
                        $"Artifact path '{artifact.Name}' is reserved for the configuration file.");
                    return result;
                }

                string target;
                try
                {
                    target = SafeTarget(tempRoot, artifact.Name);
                }
                catch (Exception ex) when (ex is ArgumentException or NotSupportedException)
                {
                    result.AddError("validator.artifact_name", ex.Message);
                    return result;
                }

                Directory.CreateDirectory(Path.GetDirectoryName(target)!);
                try
                {
                    await using var source = await artifacts.OpenReadAsync(artifact.ArtifactId, cancellationToken);
                    await using var destination = new FileStream(
                        target,
                        FileMode.Create,
                        FileAccess.Write,
                        FileShare.None,
                        bufferSize: 64 * 1024,
                        useAsync: true);
                    await source.CopyToAsync(destination, cancellationToken);
                }
                catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
                {
                    result.AddError(
                        "validator.artifact_read",
                        $"Cannot read artifact '{artifact.Name}': {ex.Message}");
                    return result;
                }
            }

            var binary = ResolveBinary(options.WasmgateBinary);
            var startInfo = new ProcessStartInfo
            {
                FileName = binary,
                WorkingDirectory = tempRoot,
                RedirectStandardOutput = true,
                RedirectStandardError = true,
                UseShellExecute = false,
                CreateNoWindow = true,
            };
            startInfo.ArgumentList.Add("-config");
            startInfo.ArgumentList.Add(configPath);
            startInfo.ArgumentList.Add("-validate");

            using var process = new Process { StartInfo = startInfo };
            try
            {
                process.Start();
            }
            catch (Exception ex)
            {
                result.AddError("validator.start_failed", $"Cannot start '{binary}': {ex.Message}");
                return result;
            }

            // The reads use no cancellation token: after a timeout the process
            // is killed and the pipes close, which lets both reads finish.
            var stdoutTask = process.StandardOutput.ReadToEndAsync();
            var stderrTask = process.StandardError.ReadToEndAsync();

            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
            timeout.CancelAfter(options.ValidationTimeout);

            try
            {
                await process.WaitForExitAsync(timeout.Token);
            }
            catch (OperationCanceledException)
            {
                TryKill(process);
                await Task.WhenAll(stdoutTask, stderrTask).ConfigureAwait(false);

                if (cancellationToken.IsCancellationRequested)
                {
                    throw;
                }

                result.AddError(
                    "validator.timeout",
                    $"wasmgate -validate did not finish within {options.ValidationTimeout}.");
                return result;
            }

            var stdout = await stdoutTask;
            var stderr = await stderrTask;

            if (process.ExitCode != 0)
            {
                var details = string.IsNullOrWhiteSpace(stderr) ? stdout : stderr;
                result.AddError(
                    "validator.invalid_config",
                    string.IsNullOrWhiteSpace(details)
                        ? $"wasmgate -validate exited with code {process.ExitCode}."
                        : details.Trim());
            }

            return result;
        }
        finally
        {
            TryDeleteDirectory(tempRoot);
        }
    }

    /// <summary>Resolves the configured validator binary path.</summary>
    /// <param name="binary">Absolute path, path with a directory separator, or command name.</param>
    /// <returns>A path usable by <see cref="ProcessStartInfo.FileName"/>.</returns>
    private static string ResolveBinary(string binary)
    {
        if (Path.IsPathRooted(binary) ||
            binary.Contains(Path.DirectorySeparatorChar) ||
            binary.Contains(Path.AltDirectorySeparatorChar))
        {
            return Path.GetFullPath(binary);
        }

        return binary;
    }

    /// <summary>Maps an artifact install path to a file inside the temporary tree.</summary>
    /// <param name="root">Temporary release root.</param>
    /// <param name="name">Artifact install path from the release.</param>
    /// <returns>Absolute target path inside <paramref name="root"/>.</returns>
    /// <exception cref="ArgumentException">The path is rooted or escapes the release directory.</exception>
    /// <exception cref="NotSupportedException">The path contains characters unsupported by the platform.</exception>
    private static string SafeTarget(string root, string name)
    {
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

    /// <summary>Kills a validator process after a timeout.</summary>
    /// <param name="process">Process to terminate.</param>
    private static void TryKill(Process process)
    {
        try
        {
            if (!process.HasExited)
            {
                process.Kill(entireProcessTree: true);
            }
        }
        catch (Exception ex) when (ex is InvalidOperationException or System.ComponentModel.Win32Exception)
        {
            // The process may have exited between the check and the kill.
        }
    }

    /// <summary>Deletes the temporary release tree, ignoring cleanup failures.</summary>
    /// <param name="path">Directory to delete.</param>
    private static void TryDeleteDirectory(string path)
    {
        try
        {
            Directory.Delete(path, recursive: true);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            // Best-effort cleanup; the temporary tree is not sensitive.
        }
    }
}
