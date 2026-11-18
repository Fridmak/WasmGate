using Microsoft.AspNetCore.Server.Kestrel.Core;
using WasmGate.Control.Api;
using WasmGate.Control.Api.Endpoints;
using WasmGate.Control.Api.Endpoints.Admin;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Api.Hosting;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Api.Infrastructure.Security;
using WasmGate.Control.Api.Observability;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Infrastructure.Storage;
using AdminJsonContext = WasmGate.Control.Api.Contracts.AdminJsonContext;

// Composition root of the control plane: options, logging, persistence, gRPC
// services and the admin HTTP API. Everything is registered as a singleton,
// because the repositories are stateless over one connection pool and the
// services only coordinate them.
//
// The content root is the application directory, not the working directory: the
// admin UI lives in wwwroot next to the binary and must be found regardless of
// where the service was started.
var builder = WebApplication.CreateSlimBuilder(new WebApplicationOptions
{
    Args = args,
    ContentRootPath = AppContext.BaseDirectory,
});

builder.SetupLogging();
builder.Services.ConfigureHttpJsonOptions(options =>
{
    options.SerializerOptions.TypeInfoResolverChain.Insert(0, AdminJsonContext.Default);
});

var options = IHostApplicationBuilder.GetCpOptions();
StartupSecurity.Validate(options);
builder.WebHost.ConfigureKestrel(kestrel =>
{
    // Artifacts travel base64-encoded inside JSON, so the request body limit is
    // derived from the artifact limit.
    kestrel.Limits.MaxRequestBodySize = checked((options.MaxArtifactBytes * 4 / 3) + (1L << 20));
    ListenEndpoint.Add(kestrel, options.GrpcListen, HttpProtocols.Http2, options.GrpcTls);
    ListenEndpoint.Add(kestrel, options.HttpListen, HttpProtocols.Http1);
});

builder.Services.AddSingleton(options);
builder.RegisterServices(options);

builder.Services.AddGrpc();
builder.Services.AddSingleton<AgentControlService>();
builder.Services.AddSingleton<AgentEnrollmentService>();
builder.Services.AddHostedService<DeploymentReaper>();

var app = builder.Build();

// Prepare the selected store before serving. SQLite creates its directory,
// takes the single-process lock and applies the embedded schema; PostgreSQL is
// a no-op and keeps its explicit migration workflow.
await app.Services.GetRequiredService<IStorageInitializer>().InitializeAsync(CancellationToken.None);

// Create the release-signing key before serving: agents read the matching
// public key when they start, and enrollment can precede the first release.
// Leaving it to the first release would make a fresh control plane unusable
// for every agent that verifies release signatures at startup.
app.Services.GetRequiredService<IReleaseSigner>();

// The admin UI is a static SPA served from wwwroot. It uses the admin API on the
// same origin, so no CORS is involved; the API itself stays protected by the
// bearer token.
app.UseDefaultFiles();
app.UseStaticFiles();

var controlMetrics = app.Services.GetRequiredService<ControlMetrics>();
app.Use(async (context, next) =>
{
    await next(context);
    if (AdminToken.IsProtectedPath(context.Request.Path))
    {
        controlMetrics.RecordHttp(context.Request.Method, context.Response.StatusCode);
    }
});

app.UseAdminToken(options.AdminToken);

app.MapGrpcService<AgentControlService>();
app.MapGrpcService<AgentEnrollmentService>();
app.MapGrpcService<ArtifactDownloadService>();
app.MapAdminEndpoints();

await app.RunAsync();
