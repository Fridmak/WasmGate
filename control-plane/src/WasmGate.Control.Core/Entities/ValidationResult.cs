namespace WasmGate.Control.Core.Entities;

/// <summary>Result of validating a configuration or a release.</summary>
public sealed class ValidationResult
{
    /// <summary>All errors and warnings produced by the validator.</summary>
    public List<ValidationIssue> Issues { get; } = [];

    /// <summary>True when no issue is marked as an error.</summary>
    public bool IsValid => Issues.TrueForAll(issue => !issue.IsError);

    /// <summary>Adds an error to the result.</summary>
    /// <param name="code">Stable machine-readable issue code.</param>
    /// <param name="message">Human-readable explanation.</param>
    public void AddError(string code, string message) =>
        Issues.Add(new ValidationIssue(code, message, IsError: true));

    /// <summary>Adds a warning to the result.</summary>
    /// <param name="code">Stable machine-readable issue code.</param>
    /// <param name="message">Human-readable explanation.</param>
    public void AddWarning(string code, string message) =>
        Issues.Add(new ValidationIssue(code, message, IsError: false));
}

/// <summary>One validation error or warning.</summary>
/// <param name="Code">Stable machine-readable issue code.</param>
/// <param name="Message">Human-readable explanation.</param>
/// <param name="IsError">True for an error, false for a warning.</param>
public sealed record ValidationIssue(string Code, string Message, bool IsError);

/// <summary>Report sent by a data plane after an apply attempt.</summary>
public sealed class ApplyReport
{
    /// <summary>Release that the data plane attempted to apply.</summary>
    public required string ReleaseId { get; init; }

    /// <summary>True when the release was applied successfully.</summary>
    public required bool Ok { get; init; }

    /// <summary>Failure reason when <see cref="Ok"/> is false.</summary>
    public string? Error { get; init; }

    /// <summary>Per-listener result of the apply.</summary>
    public IReadOnlyList<ListenerStatus> Listeners { get; init; } = [];

    /// <summary>Time when the apply finished.</summary>
    public DateTimeOffset AppliedAt { get; init; } = DateTimeOffset.UtcNow;
}

/// <summary>Result of binding one data-plane listener.</summary>
/// <param name="Name">Listener name from the configuration.</param>
/// <param name="Address">Listen address from the configuration.</param>
/// <param name="Bound">True when the listener is bound and accepting connections.</param>
/// <param name="Error">Failure reason when <paramref name="Bound"/> is false.</param>
public sealed record ListenerStatus(string Name, string Address, bool Bound, string? Error);
