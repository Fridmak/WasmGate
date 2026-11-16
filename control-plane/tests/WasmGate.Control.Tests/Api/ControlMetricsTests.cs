using WasmGate.Control.Api.Observability;
using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Tests.Api;

/// <summary>Tests for the control-plane Prometheus renderer.</summary>
public sealed class ControlMetricsTests
{
    [Fact]
    public void RendersHttpAgentsSessionsAndProcessGauges()
    {
        var metrics = new ControlMetrics();
        metrics.RecordHttp("GET", 200);
        metrics.RecordHttp("GET", 401);
        metrics.RecordHttp("post", 500);
        metrics.RecordHttp("PROPFIND", 405);
        metrics.RecordAgentQueryFailure();
        metrics.RecordAgentQueryFailure();

        var text = metrics.Render(
        [
            new Agent { Name = "edge-1", Version = "1", Environment = "prod", Status = AgentStatus.Online },
            new Agent { Name = "edge-2", Version = "1", Environment = "prod", Status = AgentStatus.Offline },
            new Agent { Name = "edge-3", Version = "1", Environment = "staging", Status = AgentStatus.Online },
        ], activeSessions: 2);

        Assert.Contains("wasmgate_control_http_requests_total{method=\"GET\",status=\"200\"} 1", text);
        Assert.Contains("wasmgate_control_http_requests_total{method=\"GET\",status=\"401\"} 1", text);
        Assert.Contains("wasmgate_control_http_requests_total{method=\"POST\",status=\"500\"} 1", text);
        Assert.Contains("wasmgate_control_http_requests_total{method=\"OTHER\",status=\"405\"} 1", text);
        Assert.Contains("wasmgate_control_agents{environment=\"prod\",status=\"Online\"} 1", text);
        Assert.Contains("wasmgate_control_agents{environment=\"prod\",status=\"Offline\"} 1", text);
        Assert.Contains("wasmgate_control_agents{environment=\"staging\",status=\"Online\"} 1", text);
        Assert.Contains("wasmgate_control_grpc_sessions_active 2", text);
        Assert.Contains("wasmgate_control_agent_query_failures_total 2", text);
        Assert.Contains("wasmgate_control_build_info{version=\"", text);
        Assert.Contains("# TYPE wasmgate_control_uptime_seconds gauge", text);
        Assert.Contains("# TYPE wasmgate_control_working_set_bytes gauge", text);
    }
}
