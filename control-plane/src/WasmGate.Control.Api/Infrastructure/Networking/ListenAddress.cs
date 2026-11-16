using System.Globalization;
using System.Net;
using System.Text;

namespace WasmGate.Control.Api.Infrastructure.Networking;

/// <summary>Transport family of a listen address.</summary>
internal enum ListenAddressKind
{
    /// <summary>TCP endpoint on an IP address, <c>localhost</c> or a wildcard.</summary>
    Tcp,

    /// <summary>Unix domain socket.</summary>
    UnixSocket,
}

/// <summary>
/// Parsed control-plane listen address.
/// </summary>
/// <remarks>
/// <para>Supported forms:</para>
/// <list type="bullet">
///   <item><c>http://&lt;ip|localhost|*&gt;:&lt;port&gt;</c> — plaintext HTTP endpoint;</item>
///   <item><c>https://&lt;ip|localhost|*&gt;:&lt;port&gt;</c> — TLS endpoint;</item>
///   <item><c>unix:///absolute/path/to.sock</c> — Unix domain socket.</item>
/// </list>
/// <para>
/// The host must be an IP literal, <c>localhost</c> or <c>*</c>; DNS names are
/// rejected so startup does not depend on resolution results. A port is always
/// required, and the path, query, fragment and userinfo parts of an HTTP URI are
/// not accepted.
/// </para>
/// </remarks>
internal sealed record ListenAddress
{
    private const string UnixPrefix = "unix://";
    private const string SchemeSeparator = "://";

    /// <summary>Transport family of this address.</summary>
    public required ListenAddressKind Kind { get; init; }

    /// <summary>True only for an <c>https://</c> address.</summary>
    public bool IsSecure { get; init; }

    /// <summary><c>*</c>, <c>localhost</c> or an IP literal; null for a socket.</summary>
    public string? Host { get; init; }

    /// <summary>TCP port; zero for a socket.</summary>
    public int Port { get; init; }

    /// <summary>Absolute socket path; null for a TCP address.</summary>
    public string? SocketPath { get; init; }

    /// <summary>
    /// True when the endpoint is reachable only from the local machine: a Unix
    /// socket, <c>localhost</c> or a loopback IP address.
    /// </summary>
    /// <remarks>
    /// The wildcard host is not local: it binds every interface.
    /// </remarks>
    public bool IsLocal =>
        Kind == ListenAddressKind.UnixSocket ||
        (Host is not null &&
         (Host.Equals("localhost", StringComparison.OrdinalIgnoreCase) ||
          (IPAddress.TryParse(Host, out var address) && IPAddress.IsLoopback(address))));

    /// <summary>Parses and validates one listen address.</summary>
    /// <param name="value">Address in one of the supported forms.</param>
    /// <returns>The parsed address.</returns>
    /// <exception cref="ArgumentException">
    /// The address is empty, has an unsupported scheme, omits the port, contains
    /// an unsupported URI component, or has an invalid host, port or socket path.
    /// </exception>
    public static ListenAddress Parse(string value)
    {
        ArgumentException.ThrowIfNullOrWhiteSpace(value);
        var text = value.Trim();

        if (text.StartsWith(UnixPrefix, StringComparison.OrdinalIgnoreCase))
        {
            return ParseUnixSocket(text, text[UnixPrefix.Length..]);
        }

        var schemeEnd = text.IndexOf(SchemeSeparator, StringComparison.Ordinal);
        if (schemeEnd <= 0)
        {
            throw Invalid(text, "expected http://, https:// or unix:///absolute/path.sock");
        }

        var scheme = text[..schemeEnd];
        var isSecure = scheme.Equals("https", StringComparison.OrdinalIgnoreCase);
        if (!isSecure && !scheme.Equals("http", StringComparison.OrdinalIgnoreCase))
        {
            var hint = scheme.Equals("tcp", StringComparison.OrdinalIgnoreCase)
                ? "use http:// or https:// for the gRPC/admin HTTP endpoint or unix:// for a socket"
                : $"unsupported scheme '{scheme}'; use http://, https:// or unix://";
            throw Invalid(text, hint);
        }

        var authorityStart = schemeEnd + SchemeSeparator.Length;
        var authorityEnd = text.IndexOfAny(['/', '?', '#'], authorityStart);
        var authority = authorityEnd < 0 ? text[authorityStart..] : text[authorityStart..authorityEnd];
        var tail = authorityEnd < 0 ? string.Empty : text[authorityEnd..];

        if (authority.Length == 0)
        {
            throw Invalid(text, "host and port are required");
        }

        if (authority.Contains('@'))
        {
            throw Invalid(text, "userinfo is not allowed");
        }

        if (tail.Length > 0 && tail != "/")
        {
            throw Invalid(text, "path, query and fragment are not allowed");
        }

        var (host, portText) = SplitAuthority(authority, text);
        if (!int.TryParse(portText, NumberStyles.None, CultureInfo.InvariantCulture, out var port) ||
            port is < 0 or > 65535)
        {
            throw Invalid(text, "port must be a number between 0 and 65535");
        }

        if (host != "*" &&
            !host.Equals("localhost", StringComparison.OrdinalIgnoreCase) &&
            !IPAddress.TryParse(host, out _))
        {
            throw Invalid(text, "host must be an IP address, 'localhost' or '*'");
        }

        return new ListenAddress
        {
            Kind = ListenAddressKind.Tcp,
            IsSecure = isSecure,
            Host = host,
            Port = port,
        };
    }

    /// <summary>Creates a configuration error with a consistent message.</summary>
    /// <param name="value">Invalid listen address.</param>
    /// <param name="reason">Human-readable reason.</param>
    /// <returns>An exception suitable for a startup failure.</returns>
    internal static ArgumentException Invalid(string value, string reason) =>
        new($"Invalid listen address '{value}': {reason}.");

    /// <summary>Validates the socket path and returns a socket address.</summary>
    /// <param name="value">Original address used in error messages.</param>
    /// <param name="path">Path component after the <c>unix://</c> prefix.</param>
    private static ListenAddress ParseUnixSocket(string value, string path)
    {
        if (path.Length == 0)
        {
            throw Invalid(value, "unix socket path is required");
        }

        if (!Path.IsPathFullyQualified(path))
        {
            throw Invalid(value, "unix socket path must be absolute");
        }

        if (Directory.Exists(path))
        {
            throw Invalid(value, "unix socket path points to a directory");
        }

        var directory = Path.GetDirectoryName(path);
        if (string.IsNullOrEmpty(directory) || !Directory.Exists(directory))
        {
            throw Invalid(
                value,
                $"unix socket directory '{directory}' does not exist; create it before starting the control plane");
        }

        if (!OperatingSystem.IsWindows())
        {
            // sun_path holds 104 bytes on macOS and 108 on Linux, including the
            // terminating NUL.
            var maxBytes = OperatingSystem.IsMacOS() ? 103 : 107;
            if (Encoding.UTF8.GetByteCount(path) > maxBytes)
            {
                throw Invalid(value, $"unix socket path is too long (maximum {maxBytes} bytes)");
            }
        }

        return new ListenAddress
        {
            Kind = ListenAddressKind.UnixSocket,
            SocketPath = path,
        };
    }

    /// <summary>Splits an HTTP authority into host and port.</summary>
    /// <param name="authority">Authority component, for example localhost:9096 or [::1]:9096.</param>
    /// <param name="value">Original address used in error messages.</param>
    /// <returns>Host without IPv6 brackets and the explicit port text.</returns>
    /// <exception cref="ArgumentException">The authority has no port or invalid IPv6 brackets.</exception>
    private static (string Host, string PortText) SplitAuthority(string authority, string value)
    {
        if (authority[0] == '[')
        {
            var close = authority.IndexOf(']');
            if (close <= 1)
            {
                throw Invalid(value, "invalid IPv6 host");
            }

            var host = authority[1..close];
            var rest = authority[(close + 1)..];
            if (!rest.StartsWith(':'))
            {
                throw Invalid(value, "port is required");
            }

            return (host, rest[1..]);
        }

        var colon = authority.LastIndexOf(':');
        if (colon <= 0)
        {
            throw Invalid(value, "port is required");
        }

        var unbracketedHost = authority[..colon];
        if (unbracketedHost.Contains(':'))
        {
            throw Invalid(value, "IPv6 hosts must be enclosed in brackets");
        }

        return (unbracketedHost, authority[(colon + 1)..]);
    }
}
