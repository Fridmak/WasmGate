using System.Text.Json;
using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// A parsed deployment wave selector.
/// </summary>
/// <remarks>
/// A selector is either the wildcard <c>*</c> or a JSON object of string label
/// values, for example <c>{"region":"eu","role":"edge"}</c>. It is parsed once,
/// before a deployment is stored, so a selector that cannot be interpreted fails
/// the request instead of silently matching nobody. Every property must be
/// present on the agent with an equal value; the comparison is ordinal and
/// case-sensitive.
/// </remarks>
internal sealed class WaveSelector
{
    /// <summary>Wildcard that targets every agent.</summary>
    public const string All = "*";

    private static readonly KeyValuePair<string, string>[] Wildcard = [];

    private readonly bool _wildcard;
    private readonly KeyValuePair<string, string>[] _labels;

    private WaveSelector(bool wildcard, KeyValuePair<string, string>[] labels)
    {
        _wildcard = wildcard;
        _labels = labels;
    }

    /// <summary>Parses and validates one selector.</summary>
    /// <param name="selector">Wildcard or a JSON object with string values.</param>
    /// <returns>The parsed selector.</returns>
    /// <exception cref="InvalidOperationException">
    /// The selector is empty, is not valid JSON, or is not an object of strings.
    /// </exception>
    public static WaveSelector Parse(string selector)
    {
        if (string.IsNullOrWhiteSpace(selector))
        {
            throw new InvalidOperationException("Wave selector is empty.");
        }

        if (selector == All)
        {
            return new WaveSelector(wildcard: true, Wildcard);
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

            var labels = new List<KeyValuePair<string, string>>();
            foreach (var property in document.RootElement.EnumerateObject())
            {
                if (property.Value.ValueKind != JsonValueKind.String)
                {
                    throw new InvalidOperationException(
                        $"Wave selector '{selector}' must contain only string label values.");
                }

                labels.Add(new KeyValuePair<string, string>(
                    property.Name,
                    property.Value.GetString()!));
            }

            return new WaveSelector(wildcard: false, [.. labels]);
        }
    }

    /// <summary>Returns true when an agent belongs to the wave.</summary>
    /// <param name="agent">Agent to test.</param>
    /// <returns>True for the wildcard, or when every label matches.</returns>
    public bool Matches(Agent agent)
    {
        if (_wildcard)
        {
            return true;
        }

        foreach (var (name, value) in _labels)
        {
            if (!agent.Labels.TryGetValue(name, out var actual) ||
                !string.Equals(actual, value, StringComparison.Ordinal))
            {
                return false;
            }
        }

        return true;
    }
}
