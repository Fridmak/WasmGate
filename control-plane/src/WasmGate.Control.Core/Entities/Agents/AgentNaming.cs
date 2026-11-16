namespace WasmGate.Control.Core.Entities.Agents;

/// <summary>
/// Validation rules shared by the admin API, enrollment and the gRPC session.
/// Keeping them in one place prevents the control plane from accepting an agent
/// name that another code path would reject.
/// </summary>
/// <remarks>
/// An agent name and the certificate CN are different values: the name is chosen
/// by the operator, the CN is always <c>wasmgate-agent:&lt;guid&gt;</c> so an
/// identity cannot be renamed on reconnect.
/// </remarks>
public static class AgentNaming
{
    /// <summary>Maximum length of an operator-visible agent name.</summary>
    public const int MaxLength = 128;

    /// <summary>Prefix of the certificate subject CN that identifies an agent.</summary>
    public const string CertificatePrefix = "wasmgate-agent:";

    /// <summary>
    /// Returns true when <paramref name="name"/> is a valid agent name.
    /// </summary>
    /// <param name="name">Candidate agent name.</param>
    /// <remarks>
    /// A name must be 1 to <see cref="MaxLength"/> characters long, must start
    /// and end with an ASCII letter or digit, and may otherwise contain ASCII
    /// letters, digits, '.', '_' and '-'. The strict character set keeps names
    /// safe for logs, URLs, selectors and database keys.
    /// </remarks>
    /// <returns>True when the name is acceptable.</returns>
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

    /// <summary>Builds the certificate subject CN for an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <returns>Common name in the form <c>wasmgate-agent:&lt;guid&gt;</c>.</returns>
    public static string CertificateCommonName(Guid agentId) =>
        CertificatePrefix + agentId.ToString("D");

    /// <summary>Parses an agent identity from a certificate subject CN.</summary>
    /// <param name="commonName">Certificate common name.</param>
    /// <param name="agentId">Parsed agent identifier.</param>
    /// <returns>True when the common name has the expected prefix and a valid GUID.</returns>
    public static bool TryParseCertificateCommonName(string? commonName, out Guid agentId)
    {
        agentId = default;
        return commonName is not null &&
               commonName.StartsWith(CertificatePrefix, StringComparison.Ordinal) &&
               Guid.TryParse(commonName.AsSpan(CertificatePrefix.Length), out agentId);
    }
}
