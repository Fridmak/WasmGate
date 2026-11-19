using System.Net;
using Microsoft.AspNetCore.Http;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Api.Infrastructure.Security;

/// <summary>
/// Rejects DNS-rebinding requests that reach an unauthenticated loopback admin API.
/// </summary>
/// <remarks>
/// <para>
/// When no admin token is configured the API is protected only by the loopback
/// bind. A browser can still be tricked into sending requests to that listener
/// through DNS rebinding: the request then carries the attacker's host name in
/// the <c>Host</c> header and is same-origin from the page's point of view.
/// Requiring a loopback <c>Host</c> on the affected paths closes that gap.
/// </para>
/// <para>
/// The check covers the same paths as <see cref="AdminToken"/>: static UI files
/// and health probes stay reachable, because they contain no administrative
/// state. A configured token makes the check unnecessary; non-loopback
/// listeners are rejected at startup unless a token is present.
/// </para>
/// </remarks>
internal static class AdminHostFilter
{
    /// <summary>Adds the loopback-host check when the admin API has no token.</summary>
    /// <param name="app">Application whose pipeline is extended.</param>
    /// <param name="options">Control-plane options with the listen address and token.</param>
    public static void UseAdminHostFilter(this IApplicationBuilder app, ControlPlaneOptions options)
    {
        if (!string.IsNullOrWhiteSpace(options.AdminToken))
        {
            return;
        }

        // StartupSecurity enforces this invariant; keep the middleware inert if
        // the options are constructed directly (for example in a test).
        if (!ListenAddress.Parse(options.HttpListen).IsLocal)
        {
            return;
        }

        app.Use(async (context, next) =>
        {
            if (!AdminToken.IsProtectedPath(context.Request.Path) ||
                IsLoopbackHost(context.Request.Host))
            {
                await next(context);
                return;
            }

            context.Response.StatusCode = StatusCodes.Status421MisdirectedRequest;
            context.Response.ContentType = "text/plain; charset=utf-8";
            await context.Response.WriteAsync(
                "host not allowed; use localhost or a loopback address");
        });
    }

    /// <summary>True for <c>localhost</c> and loopback IP literals.</summary>
    /// <param name="host">Request host header.</param>
    /// <returns>True when the host names the local machine.</returns>
    internal static bool IsLoopbackHost(HostString host)
    {
        var value = host.Host;
        if (string.IsNullOrEmpty(value))
        {
            return false;
        }

        // HostString keeps the brackets around an IPv6 literal.
        if (value.Length > 1 && value[0] == '[' && value[^1] == ']')
        {
            value = value[1..^1];
        }

        return value.Equals("localhost", StringComparison.OrdinalIgnoreCase) ||
               (IPAddress.TryParse(value, out var address) && IPAddress.IsLoopback(address));
    }
}
