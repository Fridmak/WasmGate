using System.Security.Cryptography;
using System.Text;

namespace WasmGate.Control.Api.Infrastructure.Security;

/// <summary>
/// Bearer-token protection for the admin API.
/// </summary>
/// <remarks>
/// The token is a shared secret from <c>WASMGATE_CP_ADMIN_TOKEN</c>. It is
/// compared in constant time and never logged. Health probes stay open, because
/// a load balancer must be able to check them without credentials.
/// </remarks>
internal static class AdminToken
{
    private const string Scheme = "Bearer ";

    /// <summary>True for paths that require the admin token.</summary>
    /// <param name="path">Request path.</param>
    /// <returns>True for <c>/api</c> and <c>/metrics</c> subtrees.</returns>
    /// <remarks>
    /// Segment-aware matching also covers <c>/metrics/</c>, which ASP.NET Core
    /// routes to the same endpoint as <c>/metrics</c>; an exact comparison would
    /// leave that spelling unauthenticated.
    /// </remarks>
    public static bool IsProtectedPath(PathString path) =>
        path.StartsWithSegments("/api", StringComparison.OrdinalIgnoreCase) ||
        path.StartsWithSegments("/metrics", StringComparison.OrdinalIgnoreCase);

    /// <summary>Adds the token check to every <c>/api</c> and <c>/metrics</c> request when a token is configured.</summary>
    /// <param name="app">Application whose pipeline is extended.</param>
    /// <param name="token">Configured token, or null/empty to disable authentication.</param>
    public static void UseAdminToken(this IApplicationBuilder app, string? token)
    {
        if (string.IsNullOrWhiteSpace(token))
        {
            return;
        }

        var expected = Encoding.UTF8.GetBytes(token);

        app.UseWhen(
            context => IsProtectedPath(context.Request.Path),
            branch => branch.Use(async (context, next) =>
            {
                if (IsAuthorized(context.Request.Headers.Authorization, expected))
                {
                    await next(context);
                    return;
                }

                context.Response.StatusCode = StatusCodes.Status401Unauthorized;
                context.Response.Headers.WWWAuthenticate = "Bearer";
                await context.Response.WriteAsync("unauthorized");
            }));
    }

    /// <summary>Compares the Authorization header with the configured token.</summary>
    /// <param name="header">Header value, for example <c>Bearer abc</c>.</param>
    /// <param name="expected">Configured token as UTF-8 bytes.</param>
    /// <returns>True when the scheme and the token match.</returns>
    private static bool IsAuthorized(string? header, byte[] expected)
    {
        if (header is null || !header.StartsWith(Scheme, StringComparison.OrdinalIgnoreCase))
        {
            return false;
        }

        var provided = Encoding.UTF8.GetBytes(header[Scheme.Length..].Trim());
        return CryptographicOperations.FixedTimeEquals(provided, expected);
    }
}
