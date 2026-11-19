using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Microsoft.Extensions.DependencyInjection;
using WasmGate.Control.Api.Infrastructure.Security;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// A loopback admin listener without a token must still reject a non-loopback
/// Host header, otherwise a DNS-rebinding page could call the API.
/// </summary>
public sealed class AdminHostFilterTests
{
    [Theory]
    [InlineData("localhost:9097", true)]
    [InlineData("LOCALHOST", true)]
    [InlineData("127.0.0.1:9097", true)]
    [InlineData("127.0.0.2", true)]
    [InlineData("[::1]:9097", true)]
    [InlineData("evil.example:9097", false)]
    [InlineData("192.0.2.10:9097", false)]
    [InlineData("", false)]
    public void AcceptsOnlyLoopbackHosts(string host, bool expected)
    {
        Assert.Equal(expected, AdminHostFilter.IsLoopbackHost(new HostString(host)));
    }

    [Fact]
    public async Task MiddlewareRejectsNonLoopbackApiHost()
    {
        var status = await InvokeAsync(
            new ControlPlaneOptions { HttpListen = "http://127.0.0.1:9097" },
            path: "/api/v1/agents",
            host: "evil.example:9097");

        Assert.Equal(StatusCodes.Status421MisdirectedRequest, status);
    }

    [Fact]
    public async Task MiddlewareAllowsLoopbackApiHost()
    {
        var status = await InvokeAsync(
            new ControlPlaneOptions { HttpListen = "http://127.0.0.1:9097" },
            path: "/api/v1/agents",
            host: "localhost:9097");

        Assert.Equal(StatusCodes.Status204NoContent, status);
    }

    [Fact]
    public async Task MiddlewareLeavesHealthProbeReachable()
    {
        var status = await InvokeAsync(
            new ControlPlaneOptions { HttpListen = "http://127.0.0.1:9097" },
            path: "/readyz",
            host: "evil.example:9097");

        Assert.Equal(StatusCodes.Status204NoContent, status);
    }

    [Fact]
    public async Task MiddlewareIsInactiveWhenTokenIsConfigured()
    {
        var status = await InvokeAsync(
            new ControlPlaneOptions
            {
                HttpListen = "http://127.0.0.1:9097",
                AdminToken = "configured",
            },
            path: "/api/v1/agents",
            host: "evil.example:9097");

        Assert.Equal(StatusCodes.Status204NoContent, status);
    }

    private static async Task<int> InvokeAsync(
        ControlPlaneOptions options,
        string path,
        string host)
    {
        var services = new ServiceCollection().BuildServiceProvider();
        var builder = new ApplicationBuilder(services);
        builder.UseAdminHostFilter(options);
        builder.Run(context =>
        {
            context.Response.StatusCode = StatusCodes.Status204NoContent;
            return Task.CompletedTask;
        });

        var app = builder.Build();
        var http = new DefaultHttpContext();
        http.Request.Path = path;
        http.Request.Host = new HostString(host);
        await app.Invoke(http);
        return http.Response.StatusCode;
    }
}
