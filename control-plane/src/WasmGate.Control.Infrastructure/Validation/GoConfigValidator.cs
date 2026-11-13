using System.Diagnostics;
using System.Text;
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
            var configPath = Path.Combine(tempRoot, "wasmgate.yaml");
            await File.WriteAllTextAsync(configPath, config.Yaml, cancellationToken);

            foreach (var artifact in releaseArtifacts)
            {
                var target = SafeTarget(tempRoot, artifact.Name);
                Directory.CreateDirectory(Path.GetDirectoryName(target)!);
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

            var stdoutTask = process.StandardOutput.ReadToEndAsync(cancellationToken);
            var stderrTask = process.StandardError.ReadToEndAsync(cancellationToken);
            await process.WaitForExitAsync(cancellationToken);
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
            try
            {
                Directory.Delete(tempRoot, recursive: true);
            }
            catch (IOException)
            {
                // Best-effort cleanup; the temp directory is not sensitive.
            }
        }
    }

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

    private static string SafeTarget(string root, string name)
    {
        var normalizedRoot = Path.GetFullPath(root) + Path.DirectorySeparatorChar;
        var target = Path.GetFullPath(Path.Combine(root, name));
        if (!target.StartsWith(normalizedRoot, StringComparison.Ordinal))
        {
            throw new EnrollmentException(
                EnrollmentFailure.CsrInvalid,
                $"Artifact path '{name}' escapes the release directory.");
        }

        return target;
    }
}
