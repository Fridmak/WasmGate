using Microsoft.AspNetCore.Http;
using WasmGate.Control.Api.Infrastructure.Security;

namespace WasmGate.Control.Tests.Api;

/// <summary>Tests for the admin-token path predicate.</summary>
public sealed class AdminTokenTests
{
    [Theory]
    [InlineData("/api", true)]
    [InlineData("/api/v1/agents", true)]
    [InlineData("/API/v1/agents", true)]
    [InlineData("/metrics", true)]
    [InlineData("/metrics/", true)]
    [InlineData("/Metrics", true)]
    [InlineData("/metrics/extra", true)]
    [InlineData("/apifoo", false)]
    [InlineData("/metricsfoo", false)]
    [InlineData("/healthz", false)]
    [InlineData("/readyz", false)]
    [InlineData("/", false)]
    public void ProtectsApiAndMetricsSubtrees(string path, bool expected)
    {
        Assert.Equal(expected, AdminToken.IsProtectedPath(new PathString(path)));
    }
}
