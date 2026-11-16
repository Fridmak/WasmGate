namespace WasmGate.Control.Core.Entities;

/// <summary>
/// Validation and normalisation rules for environment names.
/// </summary>
/// <remarks>
/// An environment is a deployment scope shared by configurations, releases,
/// deployments and agents. Names are stored in lower case so that an operator
/// cannot create two environments that differ only by case. The character set
/// mirrors agent names: names appear in URLs, logs and database keys.
/// </remarks>
public static class EnvironmentNaming
{
    /// <summary>Maximum length of an environment name.</summary>
    public const int MaxLength = 64;

    /// <summary>Environment used when the caller does not specify one.</summary>
    public const string Default = "prod";

    /// <summary>Trims and lower-cases an environment name.</summary>
    /// <param name="name">Candidate name, or null.</param>
    /// <returns>Normalised name; empty when the input was empty.</returns>
    public static string Normalize(string? name) =>
        (name ?? string.Empty).Trim().ToLowerInvariant();

    /// <summary>Returns true when <paramref name="name"/> is a valid environment name.</summary>
    /// <param name="name">Normalised candidate name.</param>
    /// <returns>True when the name is acceptable.</returns>
    /// <remarks>
    /// A name must be 1 to <see cref="MaxLength"/> characters long, must start
    /// and end with an ASCII letter or digit, and may otherwise contain ASCII
    /// letters, digits, '.', '_' and '-'.
    /// </remarks>
    public static bool IsValid(string? name)
    {
        if (string.IsNullOrEmpty(name) || name.Length > MaxLength)
        {
            return false;
        }

        if (!char.IsAsciiLetterOrDigit(name[0]) || !char.IsAsciiLetterOrDigit(name[^1]))
        {
            return false;
        }

        foreach (var character in name)
        {
            if (!char.IsAsciiLetterOrDigit(character) && character is not ('.' or '_' or '-'))
            {
                return false;
            }
        }

        return true;
    }
}
