namespace WasmGate.Control.Core.Entities.Releases;

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
