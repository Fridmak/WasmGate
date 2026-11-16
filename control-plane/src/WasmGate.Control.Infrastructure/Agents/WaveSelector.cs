using System.Text.Json;
using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// Matches agents against deployment wave selectors.
/// </summary>
/// <remarks>
/// A selector is either the wildcard <c>*</c> or a JSON object of string label
/// values, for example <c>{"region":"eu","role":"edge"}</c>. Every property must
/// be present on the agent with an equal value; the comparison is ordinal and
/// case-sensitive, and unknown labels simply do not match. Selectors are
/// validated before a deployment starts, so <see cref="Matches"/> can treat a
/// malformed selector as "no match" instead of throwing.
/// </remarks>
internal static class WaveSelector
{
    /// <summary>Wildcard that targets every agent.</summary>
    public const string All = "*";

    /// <summary>Validates a selector before any command is sent.</summary>
    /// <param name="selector">Wildcard or a JSON object with string values.</param>
    /// <exception cref="InvalidOperationException">
    /// The selector is empty, is not valid JSON, or is not an object of strings.
    /// </exception>
    public static void Validate(string selector)
    {
        if (string.IsNullOrWhiteSpace(selector))
        {
            throw new InvalidOperationException("Wave selector is empty.");
        }

        if (selector == All)
        {
            return;
        }

        JsonDocument document;
        try
        {
            document = JsonDocument.Parse(selector);
        }
        catch (JsonException ex)
        {
            throw new InvalidOperationException(
                $"Wave selector '{selector}' is not valid JSON: {ex.Message}",
                ex);
        }

        using (document)
        {
            if (document.RootElement.ValueKind != JsonValueKind.Object)
            {
                throw new InvalidOperationException(
                    $"Wave selector '{selector}' must be '*' or a JSON object.");
            }

            foreach (var property in document.RootElement.EnumerateObject())
            {
                if (property.Value.ValueKind != JsonValueKind.String)
                {
                    throw new InvalidOperationException(
                        $"Wave selector '{selector}' must contain only string label values.");
                }
            }
        }
    }

    /// <summary>Returns true when an agent belongs to the wave.</summary>
    /// <param name="agent">Agent to test.</param>
    /// <param name="selector">Validated wildcard or JSON label selector.</param>
    /// <returns>True when the agent matches the selector.</returns>
    public static bool Matches(Agent agent, string selector)
    {
        if (string.IsNullOrWhiteSpace(selector) || selector == All)
        {
            return true;
        }

        try
        {
            using var document = JsonDocument.Parse(selector);
            if (document.RootElement.ValueKind != JsonValueKind.Object)
            {
                return false;
            }

            foreach (var property in document.RootElement.EnumerateObject())
            {
                if (!agent.Labels.TryGetValue(property.Name, out var value) ||
                    property.Value.ValueKind != JsonValueKind.String ||
                    !string.Equals(value, property.Value.GetString(), StringComparison.Ordinal))
                {
                    return false;
                }
            }

            return true;
        }
        catch (JsonException)
        {
            return false;
        }
    }
}
